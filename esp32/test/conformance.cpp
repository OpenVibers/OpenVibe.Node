// conformance.cpp — the desktop conformance harness for ov_core.
//
// Builds with `g++ -std=c++17 -Wall -Wextra -Werror` against esp32/src/ov_core.cpp and the pinned ArduinoJson single
// header in esp32/test/.deps (run.sh fetches and checks it). It replays every fixture in
// internal/protocol/testdata/bot — each server-to-device frame is decoded and handled, and each device-to-server frame
// the core emits is checked for the fixture's keys and value types — and runs the protocol's scenarios (deadman,
// e-stop, clamping, nack codes, a job refusal, close codes, heartbeat cadence, hello/config ordering) plus the safety
// hardening scenarios (a wall clock that steps backwards, 64-bit monotonic time across the millis() wrap, wrong-typed
// latch frames, oversize/too-deep/allocation-failing frames, over-long ids and kinds, the dedup cache's ten-minute
// limit, the heartbeat cap, guarded commands while the link is lost, the backoff reset, the refused-reconnect floor,
// the pre-hello timer with the transport down, a bounded e-stop reason and an overflowing command value).
//
// Exit status is non-zero if any scenario fails; every scenario prints one PASS/FAIL line.
#include <ArduinoJson.h>

#include <algorithm>
#include <cmath>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <iostream>
#include <map>
#include <sstream>
#include <string>
#include <vector>

#include "ov_core.h"

namespace {

std::string readFile(const std::string& path) {
  std::ifstream in(path, std::ios::binary);
  std::ostringstream ss;
  ss << in.rdbuf();
  return ss.str();
}

bool parse(const std::string& text, JsonDocument& doc) { return !deserializeJson(doc, text); }

const char* tag(JsonVariantConst v) {
  if (v.is<JsonObjectConst>()) return "object";
  if (v.is<JsonArrayConst>()) return "array";
  if (v.is<bool>()) return "bool";
  if (v.is<const char*>()) return "string";
  if (v.is<long long>()) return "number";
  if (v.is<double>()) return "number";
  if (v.isNull()) return "null";
  return "other";
}

// Every key the fixture carries (v/seq/ts/type are the envelope, checked separately) must be present in the emitted
// frame with a compatible value type.
bool frameMatchesFixture(const std::string& emitted, const std::string& fixture, std::string& why) {
  JsonDocument e;
  JsonDocument f;
  if (!parse(emitted, e)) {
    why = "emitted frame is not JSON";
    return false;
  }
  if (!parse(fixture, f)) {
    why = "fixture is not JSON";
    return false;
  }
  for (JsonPairConst kv : f.as<JsonObjectConst>()) {
    const std::string key = kv.key().c_str();
    if (key == "v" || key == "seq" || key == "ts" || key == "type") continue;
    JsonVariantConst ev = e[key.c_str()];
    if (ev.isNull()) {
      why = "emitted frame is missing key " + key;
      return false;
    }
    if (std::strcmp(tag(ev), tag(kv.value())) != 0) {
      why = "key " + key + " is " + tag(ev) + ", fixture has " + tag(kv.value());
      return false;
    }
  }
  return true;
}

bool lastOfType(const std::vector<std::string>& frames, const char* type, JsonDocument& out) {
  for (auto it = frames.rbegin(); it != frames.rend(); ++it) {
    JsonDocument doc;
    if (!parse(*it, doc)) continue;
    if (std::strcmp(doc["type"] | "", type) == 0) {
      out.clear();
      out.set(doc.as<JsonVariantConst>());
      return true;
    }
  }
  return false;
}

const int64_t kT = 1738065600000LL;

std::size_t countType(const std::vector<std::string>& frames, const char* type) {
  std::size_t n = 0;
  for (const auto& s : frames) {
    JsonDocument d;
    if (parse(s, d) && std::strcmp(d["type"] | "", type) == 0) n++;
  }
  return n;
}

// Exercises the "the parser could not allocate" path on a desktop with gigabytes of free heap: flip fail and the next
// JsonDocument built by the core (in feed() or emit()) fails its allocations exactly as a full ESP32 heap would.
struct FailingAllocator : ArduinoJson::Allocator {
  bool fail = false;
  void* allocate(size_t size) override { return fail ? nullptr : std::malloc(size); }
  void deallocate(void* ptr) override { std::free(ptr); }
  void* reallocate(void* ptr, size_t new_size) override { return fail ? nullptr : std::realloc(ptr, new_size); }
};

struct H {
  int64_t now = kT;       // monotonic milliseconds: every core timer runs on this clock
  int64_t wall_shift = 0; // Unix milliseconds = now + wall_shift; only the envelope ts/at/heartbeat t read it
  FailingAllocator alloc;       // the frame/envelope seam (feed and emit)
  FailingAllocator value_alloc; // the command-value seam (clamp_command and the actuator re-parse)
  std::vector<std::string> sent;
  std::vector<std::string> drives;
  std::vector<std::string> stops;
  std::vector<std::string> actuator_names;
  std::vector<std::string> actuator_values;
  std::vector<std::string> logs;
  std::string last_drive;
  std::string last_actuator_name;
  std::string last_actuator_value;
  int last_deadline = 0;
  ov::Core core;

  H()
      : core([this]() { return now; }, [this]() { return now + wall_shift; },
             [this](const std::string& s) { sent.push_back(s); }) {
    core.set_json_allocator(&alloc);
    core.set_value_allocator(&value_alloc);
    core.set_descriptor("openvibe-esp32-0.1.0", "esp32", "{\"esp32\":{\"drive\":{\"type\":\"differential\"}}}");
    core.set_local_limits(1.0, 1.0, 1000);
    core.set_callbacks(
        [this](const std::string& v, int d) {
          last_drive = v;
          last_deadline = d;
          drives.push_back(v);
        },
        [this](const std::string& n, const std::string& v, int /*d*/) {
          last_actuator_name = n;
          last_actuator_value = v;
          actuator_names.push_back(n);
          actuator_values.push_back(v);
        },
        [this]() { stops.push_back("stop"); },
        [this](const char* l, const std::string& m) { logs.push_back(std::string(l) + ": " + m); });
  }

  bool hasFrame(const char* type) {
    JsonDocument d;
    return lastOfType(sent, type, d);
  }
  bool lastNack(const std::string& id, std::string& fault, std::string& message) {
    for (auto it = sent.rbegin(); it != sent.rend(); ++it) {
      JsonDocument d;
      if (!parse(*it, d)) continue;
      if (std::strcmp(d["type"] | "", "nack") != 0) continue;
      if (std::string(d["id"] | "") != id) continue;
      fault = d["fault_code"] | "";
      message = d["message"] | "";
      return true;
    }
    return false;
  }
};

std::string commandFrame(const std::string& id, const std::string& kind, const std::string& value_json, int64_t ts,
                         int64_t deadline) {
  return "{\"v\":1,\"seq\":1,\"ts\":" + std::to_string(ts) + ",\"type\":\"command\",\"id\":\"" + id +
         "\",\"kind\":\"" + kind + "\",\"value\":" + value_json + ",\"deadline_ms\":" + std::to_string(deadline) + "}";
}

std::string configFrameRaw(double max_speed, double max_turn, int max_command_ms, int heartbeat_ms,
                           const std::string& estop_json) {
  return "{\"v\":1,\"seq\":1,\"ts\":1,\"type\":\"config\",\"heartbeat_ms\":" + std::to_string(heartbeat_ms) +
         ",\"limits\":{\"max_speed\":" + std::to_string(max_speed) + ",\"max_turn\":" + std::to_string(max_turn) +
         ",\"max_command_ms\":" + std::to_string(max_command_ms) +
         "},\"allowed_commands\":[\"drive\",\"actuator\",\"halt\"],\"estop_latched\":" + estop_json + "}";
}

std::string configFrame(double max_speed, double max_turn, int max_command_ms, int heartbeat_ms) {
  return configFrameRaw(max_speed, max_turn, max_command_ms, heartbeat_ms, "false");
}

#define REQUIRE(cond, msg)          \
  do {                              \
    if (!(cond)) return std::string(msg); \
  } while (0)

std::string scenarioHello(const std::string& hello, const std::string& config) {
  H h;
  h.now = kT;
  h.core.connect();
  REQUIRE(!h.core.link_up(), "link counted up before hello");
  REQUIRE(h.sent.empty(), "the device sent a frame before hello");
  h.core.feed(hello);
  REQUIRE(h.core.link_up(), "hello did not bring the link up");
  REQUIRE(h.core.device_id() == "dev_01J8Z4…", "hello did not record the device id");
  REQUIRE(h.core.robot_ids().size() == 1, "hello did not record the robot id");
  REQUIRE(h.sent.empty(), "the device sent a frame between hello and config");
  h.core.feed(config);
  REQUIRE(h.core.configured(), "config was not applied");
  JsonDocument status;
  REQUIRE(lastOfType(h.sent, "status", status), "no status went out after config");
  REQUIRE(lastOfType(h.sent, "estop_state", status), "no estop_state went out after config");
  JsonDocument status2;
  lastOfType(h.sent, "status", status2);
  REQUIRE(std::strcmp(status2["firmware"] | "", "openvibe-esp32-0.1.0") == 0, "status.firmware wrong");
  REQUIRE(status2["capabilities"].is<JsonObjectConst>(), "status.capabilities is not an object");
  REQUIRE(status2["capabilities"]["worker"].isNull(), "status advertised a worker capability");
  return "";
}

std::string scenarioHeartbeat(const std::string& hello, const std::string& config) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  h.sent.clear();
  h.core.tick();
  JsonDocument hb;
  REQUIRE(lastOfType(h.sent, "heartbeat", hb), "no heartbeat after hello");
  std::size_t count = 0;
  for (const auto& s : h.sent) {
    JsonDocument d;
    if (parse(s, d) && std::strcmp(d["type"] | "", "heartbeat") == 0) count++;
  }
  REQUIRE(count == 1, "expected one heartbeat, got " + std::to_string(count));
  REQUIRE(hb["seq"].is<long long>(), "heartbeat.seq is not a number");
  REQUIRE(hb["t"].is<long long>(), "heartbeat.t is not a number");
  h.core.tick();
  h.now = kT + 999;
  h.core.tick();
  std::size_t count2 = 0;
  for (const auto& s : h.sent) {
    JsonDocument d;
    if (parse(s, d) && std::strcmp(d["type"] | "", "heartbeat") == 0) count2++;
  }
  REQUIRE(count2 == 1, "a heartbeat went out before the interval");
  h.now = kT + 1000;
  h.core.tick();
  std::size_t count3 = 0;
  for (const auto& s : h.sent) {
    JsonDocument d;
    if (parse(s, d) && std::strcmp(d["type"] | "", "heartbeat") == 0) count3++;
  }
  REQUIRE(count3 == 2, "no heartbeat at the interval");
  JsonDocument hb_latest;
  REQUIRE(lastOfType(h.sent, "heartbeat", hb_latest), "no second heartbeat");
  const int64_t t2 = hb_latest["t"].as<long long>();
  // The newer echo/t shape measures the round trip.
  h.now = kT + 1050;
  h.core.feed("{\"v\":1,\"seq\":99,\"ts\":" + std::to_string(h.now) + ",\"type\":\"heartbeat_ack\",\"echo\":" +
              std::to_string(t2) + ",\"t\":" + std::to_string(t2) + "}");
  h.sent.clear();
  h.now = kT + 2000;
  h.core.tick();
  JsonDocument hb2;
  REQUIRE(lastOfType(h.sent, "heartbeat", hb2), "no heartbeat with rtt");
  REQUIRE(hb2["rtt_ms"].is<long long>(), "rtt_ms missing after an echo ack");
  REQUIRE(hb2["rtt_ms"].as<long long>() == 50, "the echoed send time measured " + std::to_string(hb2["rtt_ms"].as<long long>()) + " ms");
  // The older seq shape is matched too.
  H h2;
  h2.now = kT;
  h2.core.connect();
  h2.core.feed(hello);
  h2.core.feed(config);
  h2.sent.clear();
  h2.core.tick();
  JsonDocument hb3;
  REQUIRE(lastOfType(h2.sent, "heartbeat", hb3), "no heartbeat in the seq-shape core");
  const int64_t seq3 = hb3["seq"].as<long long>();
  h2.now = kT + 20;
  h2.core.feed("{\"v\":1,\"seq\":41,\"ts\":" + std::to_string(h2.now) + ",\"type\":\"heartbeat_ack\",\"seq\":" +
               std::to_string(seq3) + "}");
  h2.sent.clear();
  h2.now = kT + 1000;
  h2.core.tick();
  JsonDocument hb4;
  REQUIRE(lastOfType(h2.sent, "heartbeat", hb4), "no heartbeat after a seq-shape ack");
  REQUIRE(hb4["rtt_ms"].is<long long>(), "rtt_ms missing after a seq-shape ack");
  return "";
}

std::string scenarioDeadman(const std::string& hello, const std::string& config, const std::string& drive_fixture) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  h.core.feed(drive_fixture);
  REQUIRE(h.drives.size() == 1, "the drive fixture did not reach on_drive");
  REQUIRE(h.stops.empty(), "motion stopped when the drive arrived");
  h.now = kT + 299;
  h.core.tick();
  REQUIRE(h.stops.empty(), "motion stopped before the window lapsed");
  h.now = kT + 300;
  h.core.tick();
  REQUIRE(h.stops.size() == 1, "motion did not stop when the window lapsed");
  h.now = kT + 400;
  h.core.tick();
  REQUIRE(h.stops.size() == 1, "motion stopped more than once");
  return "";
}

std::string scenarioEstop(const std::string& hello, const std::string& config) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  h.sent.clear();
  h.core.feed("{\"v\":1,\"seq\":5,\"ts\":" + std::to_string(h.now) +
              ",\"type\":\"estop\",\"latched\":true,\"by\":\"usr_1\",\"at\":\"2026-09-29T19:20:01.200Z\"}");
  REQUIRE(h.core.estopped(), "the e-stop did not latch");
  REQUIRE(!h.stops.empty(), "the e-stop did not stop motion");
  JsonDocument st;
  REQUIRE(lastOfType(h.sent, "estop_state", st), "no estop_state on latch");
  REQUIRE(st["latched"].as<bool>(), "estop_state did not report the latch");
  const std::string id = "cmd_estopped";
  h.core.feed(commandFrame(id, "drive", "{\"throttle\":0.5,\"steer\":0}", h.now, h.now + 300));
  std::string fault;
  std::string message;
  REQUIRE(h.lastNack(id, fault, message), "a drive while e-stopped was not nacked");
  REQUIRE(fault == "estopped", "drive while e-stopped nacked " + fault);
  REQUIRE(h.drives.empty(), "a drive reached the motors while e-stopped");
  h.core.feed("{\"v\":1,\"seq\":6,\"ts\":" + std::to_string(h.now) + ",\"type\":\"estop\",\"latched\":false}");
  REQUIRE(!h.core.estopped(), "the e-stop did not clear");
  h.core.feed(commandFrame("cmd_after_clear", "drive", "{\"throttle\":0.5,\"steer\":0}", h.now, h.now + 300));
  REQUIRE(h.drives.size() == 1, "a drive after the clear did not reach the motors");
  return "";
}

std::string scenarioClamp(const std::string& hello) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(configFrame(0.5, 0.4, 1000, 1000));
  h.core.feed(commandFrame("cmd_clamp", "drive", "{\"throttle\":0.9,\"steer\":-0.8,\"x\":2.0,\"y\":-2.0,\"rotation\":1.5}",
                           h.now, h.now + 300));
  JsonDocument d;
  REQUIRE(parse(h.last_drive, d), "on_drive did not get JSON");
  REQUIRE(std::fabs(d["throttle"].as<double>() - 0.5) < 1e-6, "throttle was not clamped to max_speed");
  REQUIRE(std::fabs(d["steer"].as<double>() + 0.4) < 1e-6, "steer was not clamped to max_turn");
  REQUIRE(std::fabs(d["x"].as<double>() - 0.5) < 1e-6, "x was not clamped to max_speed");
  REQUIRE(std::fabs(d["y"].as<double>() + 0.5) < 1e-6, "y was not clamped to max_speed");
  REQUIRE(std::fabs(d["rotation"].as<double>() - 0.4) < 1e-6, "rotation was not clamped to max_turn");
  // The stricter of the server's config and the local caps wins.
  h.core.set_local_limits(0.25, 0.25, 1000);
  h.core.feed(configFrame(0.5, 0.5, 1000, 1000));
  h.core.feed(commandFrame("cmd_clamp2", "drive", "{\"throttle\":0.9,\"steer\":0.9}", h.now, h.now + 300));
  REQUIRE(parse(h.last_drive, d), "second on_drive was not JSON");
  REQUIRE(std::fabs(d["throttle"].as<double>() - 0.25) < 1e-6, "the local max_speed was not the stricter one");
  REQUIRE(std::fabs(d["steer"].as<double>() - 0.25) < 1e-6, "the local max_turn was not the stricter one");
  return "";
}

std::string scenarioNacks(const std::string& hello, const std::string& config) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  std::string fault;
  std::string message;
  h.core.feed(commandFrame("cmd_unknown", "jump", "{}", h.now, h.now + 300));
  REQUIRE(h.lastNack("cmd_unknown", fault, message), "unknown kind was not nacked");
  REQUIRE(fault == "unsupported", "unknown kind nacked " + fault);
  h.core.feed(commandFrame("cmd_badvalue", "drive", "{\"throttle\":\"fast\"}", h.now, h.now + 300));
  REQUIRE(h.lastNack("cmd_badvalue", fault, message), "a malformed value was not nacked");
  REQUIRE(fault == "bad_value", "malformed value nacked " + fault);
  // A repeated id is answered with the first result and never runs again.
  const std::size_t runs = h.drives.size();
  h.core.feed(commandFrame("cmd_repeat", "drive", "{\"throttle\":0.3}", h.now, h.now + 300));
  REQUIRE(h.drives.size() == runs + 1, "the first command did not run");
  h.core.feed(commandFrame("cmd_repeat", "drive", "{\"throttle\":0.9}", h.now, h.now + 300));
  REQUIRE(h.drives.size() == runs + 1, "a repeated id ran a second time");
  return "";
}

std::string scenarioJob(const std::string& hello, const std::string& config) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  const std::string id = "job_01J8Z4M2Q0R7T9YV3K6N8P1W2X";
  const std::string job = "{\"v\":1,\"seq\":1,\"ts\":" + std::to_string(h.now) +
                          ",\"type\":\"job\",\"job\":{\"id\":\"" + id +
                          "\",\"class\":\"function\",\"artifact\":{\"name\":\"hello\",\"version\":\"1.0.0\"},"
                          "\"args\":{},\"ttl_ms\":1000,\"limits\":{\"wall_ms\":1000,\"cpu_ms\":1000,\"mem_bytes\":1048576}}}";
  h.core.feed(job);
  std::string fault;
  std::string message;
  REQUIRE(h.lastNack(id, fault, message), "a job was not nacked");
  REQUIRE(fault == "unsupported", "a job nacked " + fault);
  REQUIRE(message == "class not available", "a job nacked with: " + message);
  h.core.feed("{\"v\":1,\"seq\":2,\"ts\":" + std::to_string(h.now) +
              ",\"type\":\"job\",\"job\":{\"id\":\"nope\",\"class\":\"function\"}}");
  REQUIRE(h.lastNack("nope", fault, message), "a malformed job id was not nacked");
  REQUIRE(fault == "bad_value" && message == "missing or malformed job id", "malformed job id nacked " + fault + "/" + message);
  return "";
}

std::string scenarioClose(const std::string& hello, const std::string& config) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  h.core.feed(commandFrame("cmd_close", "drive", "{\"throttle\":0.5}", h.now, h.now + 300));
  REQUIRE(h.drives.size() == 1, "the drive before the close did not run");
  h.stops.clear();
  h.core.on_close(4003);
  REQUIRE(!h.stops.empty(), "close 4003 did not stop motion");
  REQUIRE(h.core.credential_refused(), "close 4003 did not mark the credential refused");
  REQUIRE(h.core.next_reconnect_delay_ms() >= 10000, "close 4003 reconnects sooner than 10 s");
  bool remedy = false;
  for (const auto& l : h.logs) {
    if (l.find("pair again, or import the owner's rotation") != std::string::npos) remedy = true;
  }
  REQUIRE(remedy, "close 4003 did not log the two remedies");

  H h2;
  h2.now = kT;
  h2.core.connect();
  h2.core.feed(hello);
  h2.core.feed(config);
  h2.core.on_close(4002);
  REQUIRE(h2.core.next_reconnect_delay_ms() >= 10000, "close 4002 reconnects sooner than 10 s");

  H h3;
  h3.now = kT;
  h3.core.connect();
  h3.core.feed(hello);
  h3.core.feed(config);
  h3.core.on_close(4000);
  REQUIRE(!h3.core.credential_refused(), "close 4000 was treated as a refused credential");
  REQUIRE(h3.core.next_reconnect_delay_ms() < 10000, "close 4000 used the credential retry floor");
  return "";
}

std::string scenarioClock(const std::string& hello, const std::string& config) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  const int64_t t0 = h.now;
  h.core.feed(commandFrame("cmd_clock1", "drive", "{\"throttle\":0.5}", t0, t0 + 300));
  REQUIRE(h.drives.size() == 1, "the drive before the clock step did not run");

  // The wall clock steps back an hour. The envelope ts follows it; no safety timer may.
  h.wall_shift = -3600000;
  ov::Telemetry telemetry;
  telemetry.has_battery = true;
  telemetry.battery = 0.5;
  h.sent.clear();
  h.core.send_telemetry(telemetry);
  JsonDocument d;
  REQUIRE(lastOfType(h.sent, "telemetry", d), "no telemetry after the wall clock stepped back");
  REQUIRE(d["ts"].as<int64_t>() == h.now - 3600000, "the envelope ts did not follow the wall clock backwards");

  h.now = t0 + 299;
  h.core.tick();
  REQUIRE(h.stops.empty(), "the motion deadline moved with the wall clock");
  h.now = t0 + 300;
  h.core.tick();
  REQUIRE(h.stops.size() == 1, "the motion deadline did not fire on the monotonic clock");

  // A command after the step is still timed against the monotonic clock and the skew estimate, not the wall clock.
  h.core.feed(commandFrame("cmd_clock2", "drive", "{\"throttle\":0.5}", h.now, h.now + 300));
  REQUIRE(h.drives.size() == 2, "a drive after the wall clock step did not run");
  h.now += 300;
  h.core.tick();
  REQUIRE(h.stops.size() == 2, "the second deadline did not fire after the wall clock step");

  // The heartbeat cadence and the deadman still run on the monotonic clock after the step.
  h.sent.clear();
  h.now += 2001;
  h.core.tick();
  JsonDocument hb;
  REQUIRE(lastOfType(h.sent, "heartbeat", hb), "no heartbeat after the wall clock step");
  REQUIRE(hb["t"].as<int64_t>() == h.now - 3600000, "heartbeat.t did not follow the wall clock");
  REQUIRE(h.core.link_lost(), "the deadman moved with the wall clock");
  return "";
}

std::string scenarioMonotonic64(const std::string& hello, const std::string& config) {
  H h;
  h.now = 4294967000LL; // just below the 32-bit millis() wrap at 49.7 days
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  const int64_t t0 = h.now;
  // The server's clock is Unix (the hello ts); the device's monotonic clock is the wrap point. Deadlines ride the
  // server's clock, the timers the device's.
  const auto server_ms = [&]() { return kT + (h.now - t0); };
  h.core.feed(commandFrame("cmd_wrap", "drive", "{\"throttle\":0.4}", server_ms(), server_ms() + 300));
  REQUIRE(h.drives.size() == 1, "the drive before the 32-bit wrap did not run");
  h.sent.clear();
  h.now = t0 + 296; // exactly 2^32
  h.core.tick();
  REQUIRE(h.stops.empty(), "motion stopped early across the 32-bit wrap");
  h.now = t0 + 300;
  h.core.tick();
  REQUIRE(h.stops.size() == 1, "the motion deadline did not fire across the 32-bit wrap");
  JsonDocument hb;
  REQUIRE(lastOfType(h.sent, "heartbeat", hb), "no heartbeat across the 32-bit wrap");
  REQUIRE(hb["t"].as<int64_t>() >= 4294967000LL, "the heartbeat lost the 64-bit monotonic time");
  return "";
}

std::string scenarioWrongTypedLatch(const std::string& hello, const std::string& config) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  const std::string estop_true = "{\"v\":1,\"seq\":5,\"ts\":" + std::to_string(h.now) +
                                 ",\"type\":\"estop\",\"latched\":true,\"by\":\"usr_1\"}";
  h.core.feed(estop_true);
  REQUIRE(h.core.estopped(), "the e-stop did not latch");
  for (const std::string& raw : {std::string("\"false\""), std::string("0"), std::string("1"), std::string("{}")}) {
    h.core.feed("{\"v\":1,\"seq\":6,\"ts\":" + std::to_string(h.now) + ",\"type\":\"estop\",\"latched\":" + raw + "}");
    REQUIRE(h.core.estopped(), "estop latched:" + raw + " released the latch");
  }
  h.core.feed(commandFrame("cmd_wrongtype", "drive", "{\"throttle\":0.5}", h.now, h.now + 300));
  std::string fault;
  std::string message;
  REQUIRE(h.lastNack("cmd_wrongtype", fault, message) && fault == "estopped",
          "a drive while latched was not nacked estopped");
  REQUIRE(h.drives.empty(), "a drive reached the motors while latched");
  h.core.feed("{\"v\":1,\"seq\":9,\"ts\":" + std::to_string(h.now) + ",\"type\":\"estop\",\"latched\":false}");
  REQUIRE(!h.core.estopped(), "a proper clear did not release the latch");

  // The same for config.estop_latched: a wrong-typed frame must apply nothing, including its heartbeat_ms.
  h.core.feed(estop_true);
  REQUIRE(h.core.estopped(), "the second latch did not take");
  h.core.tick();
  for (const std::string& raw : {std::string("\"false\""), std::string("0"), std::string("1"), std::string("{}")}) {
    h.core.feed(configFrameRaw(1.0, 1.0, 1000, 4000, raw));
    REQUIRE(h.core.estopped(), "config estop_latched:" + raw + " released the latch");
  }
  h.now += 1000;
  h.core.tick();
  h.sent.clear();
  h.now += 1000;
  h.core.tick();
  REQUIRE(countType(h.sent, "heartbeat") == 1,
          "a wrong-typed config was still applied (its heartbeat_ms took effect)");
  h.core.feed(configFrame(1.0, 1.0, 1000, 1000));
  REQUIRE(!h.core.estopped(), "a proper config did not clear the latch");
  return "";
}

std::string scenarioFrameLimits(const std::string& hello, const std::string& config) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);

  // A frame past the device's size limit: too big to be any frame this device handles (an estop could be inside).
  h.core.feed(commandFrame("cmd_limits1", "drive", "{\"throttle\":0.5}", h.now, h.now + 300));
  REQUIRE(h.drives.size() == 1, "the drive before the oversize frame did not run");
  h.stops.clear();
  h.core.feed(std::string(ov::kMaxFrameBytes + 1, 'x'));
  REQUIRE(h.stops.size() == 1, "an oversize frame did not stop motion");
  REQUIRE(h.core.link_lost(), "an oversize frame did not drop the link");

  // A frame nested past the JSON depth limit.
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  h.core.feed(commandFrame("cmd_limits2", "drive", "{\"throttle\":0.5}", h.now, h.now + 300));
  REQUIRE(h.drives.size() == 2, "the drive before the too-deep frame did not run");
  h.stops.clear();
  std::string deep;
  for (int i = 0; i < 14; ++i) deep += "[";
  deep += "1";
  for (int i = 0; i < 14; ++i) deep += "]";
  h.core.feed(deep);
  REQUIRE(h.stops.size() == 1, "a too-deep frame did not stop motion");
  REQUIRE(h.core.link_lost(), "a too-deep frame did not drop the link");

  // A frame the parser cannot allocate room for.
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  h.core.feed(commandFrame("cmd_limits3", "drive", "{\"throttle\":0.5}", h.now, h.now + 300));
  REQUIRE(h.drives.size() == 3, "the drive before the allocation-failing frame did not run");
  h.stops.clear();
  h.alloc.fail = true;
  h.core.feed("{\"v\":1,\"seq\":9,\"ts\":" + std::to_string(h.now) +
              ",\"type\":\"estop\",\"latched\":true,\"by\":\"usr_1\"}");
  h.alloc.fail = false;
  REQUIRE(h.stops.size() == 1, "a frame that failed to allocate did not stop motion");
  REQUIRE(h.core.link_lost(), "a frame that failed to allocate did not drop the link");

  // An outbound frame that lost fields to a failed allocation is dropped, never sent half-formed.
  ov::Telemetry telemetry;
  telemetry.has_battery = true;
  telemetry.battery = 0.5;
  h.sent.clear();
  h.alloc.fail = true;
  h.core.send_telemetry(telemetry);
  h.alloc.fail = false;
  REQUIRE(h.sent.empty(), "a frame that lost fields to allocation failure was sent");
  return "";
}

std::string scenarioLongIds(const std::string& hello, const std::string& config) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  h.sent.clear();
  h.core.feed(commandFrame(std::string(200, 'a'), "drive", "{\"throttle\":0.5}", h.now, h.now + 300));
  REQUIRE(h.drives.empty(), "a command with a 200-byte id reached the motors");
  REQUIRE(h.sent.empty(), "a command with a 200-byte id was answered or echoed");

  const std::string big_kind = std::string("k") + std::string(150, 'k');
  h.core.feed(commandFrame("cmd_bigkind", big_kind, "{}", h.now, h.now + 300));
  std::string fault;
  std::string message;
  REQUIRE(h.lastNack("cmd_bigkind", fault, message), "a 151-byte kind was not nacked");
  REQUIRE(fault == "unsupported", "a 151-byte kind nacked " + fault);
  REQUIRE(message == "unknown kind", "the cached nack echoes the kind: " + message);
  h.sent.clear();
  h.core.feed(commandFrame("cmd_bigkind", big_kind, "{}", h.now, h.now + 300));
  REQUIRE(h.lastNack("cmd_bigkind", fault, message) && message == "unknown kind", "the cached nack changed");
  return "";
}

std::string scenarioDedupTtl(const std::string& hello, const std::string& config) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  const std::string id = "cmd_ttl";
  h.core.feed(commandFrame(id, "drive", "{\"throttle\":0.3}", h.now, h.now + 300));
  REQUIRE(h.drives.size() == 1, "the first command did not run");
  h.core.feed(commandFrame(id, "drive", "{\"throttle\":0.9}", h.now, h.now + 300));
  REQUIRE(h.drives.size() == 1, "a repeat inside ten minutes ran again");

  // The cache lives across reconnects (protocol §Commands step 1).
  h.core.on_close(4000);
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  h.core.feed(commandFrame(id, "drive", "{\"throttle\":0.9}", h.now, h.now + 300));
  REQUIRE(h.drives.size() == 1, "a repeat after a reconnect ran again");

  // ... and expires after the spec's ten minutes.
  h.now += ov::kDedupTTLMS + 1;
  h.core.feed(commandFrame(id, "drive", "{\"throttle\":0.2}", h.now, h.now + 300));
  REQUIRE(h.drives.size() == 2, "an id older than ten minutes was still answered from the cache");
  return "";
}

std::string scenarioHeartbeatCap(const std::string& hello) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(configFrameRaw(1.0, 1.0, 1000, 2000000000, "false")); // a server trying to stretch the deadman for weeks
  h.sent.clear();
  h.core.tick();
  REQUIRE(countType(h.sent, "heartbeat") == 1, "no heartbeat after hello");
  h.now = kT + 4999;
  h.core.tick();
  REQUIRE(countType(h.sent, "heartbeat") == 1, "a heartbeat went out before the capped interval");
  h.now = kT + 5000;
  h.core.tick();
  REQUIRE(countType(h.sent, "heartbeat") == 2, "the heartbeat interval was not capped at 5000 ms");
  h.stops.clear();
  h.now = kT + 10000;
  h.core.tick();
  REQUIRE(!h.core.link_lost(), "the deadman fired before 2 x the capped heartbeat");
  h.now = kT + 10001;
  h.core.tick();
  REQUIRE(h.core.link_lost(), "the deadman did not fire at 2 x the capped heartbeat (overflow?)");
  return "";
}

std::string scenarioLinkLostGuard(const std::string& hello, const std::string& config, const std::string& drive_fixture) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  h.core.feed(drive_fixture);
  REQUIRE(h.drives.size() == 1, "the drive before the deadman did not run");
  h.core.tick();
  h.now += 2001;
  h.core.tick();
  REQUIRE(h.core.link_lost(), "the deadman did not drop the link");
  h.stops.clear();
  h.core.feed(commandFrame("cmd_lost", "drive", "{\"throttle\":0.5}", h.now, h.now + 300));
  std::string fault;
  std::string message;
  REQUIRE(h.lastNack("cmd_lost", fault, message), "a guarded command was not answered while the link is lost");
  REQUIRE(fault == "not_connected", "a guarded command while the link is lost nacked " + fault);
  REQUIRE(h.stops.empty(), "a guarded command reached the motors while the link is lost");
  h.core.feed(commandFrame("cmd_lost_halt", "halt", "{}", h.now, h.now + 300));
  bool acked = false;
  for (const auto& frame : h.sent) {
    JsonDocument d;
    if (parse(frame, d) && std::strcmp(d["type"] | "", "ack") == 0 && std::string(d["id"] | "") == "cmd_lost_halt") {
      acked = true;
    }
  }
  REQUIRE(acked, "halt was refused while the link is lost");
  return "";
}

std::string scenarioBackoffReset(const std::string& hello) {
  H h;
  h.now = kT;
  int max_delay = 0;
  for (int i = 0; i < 10; ++i) {
    h.core.connect();
    h.core.feed(hello);
    h.core.on_close(0); // an unknown close after hello: the normal exponential backoff
    max_delay = std::max(max_delay, h.core.next_reconnect_delay_ms());
  }
  REQUIRE(max_delay > ov::kBackoffMinMS + 500, "the backoff never grew across ten drops");
  // A link that stays up for kStableLinkMS resets it: the next drop is back at the minimum.
  h.core.connect();
  h.core.feed(hello);
  h.now += ov::kStableLinkMS;
  h.core.tick();
  h.core.on_close(0);
  REQUIRE(h.core.next_reconnect_delay_ms() <= ov::kBackoffMinMS + 500, "the backoff was not reset after a stable link");
  return "";
}

std::string scenarioRefusedFloor(const std::string& hello) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.on_close(4003);
  REQUIRE(h.core.next_reconnect_delay_ms() >= ov::kCredentialRetryMinMS, "4003 used less than the 10 s floor");
  // The reconnect is refused again before hello; the transport reports code 0 ("the library cannot know").
  h.core.connect();
  h.core.on_close(0);
  REQUIRE(h.core.next_reconnect_delay_ms() >= ov::kCredentialRetryMinMS,
          "a reconnect refused before hello used less than the 10 s floor");
  REQUIRE(h.core.credential_refused(), "a pre-hello drop was not marked as the refused case");
  bool uncertain = false;
  for (const auto& l : h.logs) {
    if (l.find("cannot know the close code") != std::string::npos) uncertain = true;
  }
  REQUIRE(uncertain, "the pre-hello drop did not say the cause is uncertain");
  // An unknown close after hello is an ordinary drop and keeps the ordinary backoff.
  H h2;
  h2.now = kT;
  h2.core.connect();
  h2.core.feed(hello);
  h2.core.on_close(0);
  REQUIRE(h2.core.next_reconnect_delay_ms() < ov::kCredentialRetryMinMS,
          "an unknown close after hello used the credential floor");
  return "";
}

std::string scenarioPreHelloTimer() {
  H h;
  h.now = kT;
  // While the socket is up and no hello has arrived, the pre-hello allowance still fires: the device must not wait
  // forever on a server that upgraded the socket and then said nothing.
  h.core.connect();
  h.now = kT + ov::kHelloAllowanceMS + 1;
  h.core.tick();
  REQUIRE(h.core.link_lost(), "the pre-hello timer did not fire while the socket was up");
  h.core.clear_link_lost();
  // Once the transport is down (the socket ended) no timer may fire: nothing can arrive, and a firing timer would
  // cancel the in-flight reconnect on every loop() pass and flood the log. on_close() is what tells the core.
  h.core.on_close(0);
  h.stops.clear();
  h.sent.clear();
  for (int i = 0; i < 20; ++i) {
    h.now += 1000;
    h.core.tick();
  }
  REQUIRE(!h.core.link_lost(), "the pre-hello timer fired with the transport down");
  REQUIRE(h.stops.empty(), "a tick with the transport down stopped motion");
  return "";
}

std::string scenarioEstopReasonBound(const std::string& hello, const std::string& config) {
  H h;
  h.now = kT;
  h.core.connect();
  h.core.feed(hello);
  h.core.feed(config);
  h.sent.clear();
  const std::string big_by(16000, 'x'); // under the frame limit, far over the reason cap
  h.core.feed("{\"v\":1,\"seq\":5,\"ts\":" + std::to_string(h.now) +
              ",\"type\":\"estop\",\"latched\":true,\"by\":\"" + big_by + "\"}");
  REQUIRE(h.core.estopped(), "the e-stop with a huge by did not latch");
  JsonDocument st;
  REQUIRE(lastOfType(h.sent, "estop_state", st), "no estop_state for the huge-by latch");
  const std::string reason = st["reason"] | "";
  REQUIRE(reason.size() <= ov::kMaxEstopReasonBytes,
          "estop_state.reason is " + std::to_string(reason.size()) + " bytes");
  // The same bounded reason is copied into the dedup cache on the estopped nack, so the cache cannot grow to megabytes.
  std::string fault;
  std::string message;
  h.core.feed(commandFrame("cmd_bigreason", "drive", "{\"throttle\":0.5}", h.now, h.now + 300));
  REQUIRE(h.lastNack("cmd_bigreason", fault, message), "a drive after the huge-by latch was not nacked");
  REQUIRE(fault == "estopped", "the drive after the huge-by latch nacked " + fault);
  REQUIRE(message.size() <= ov::kMaxEstopReasonBytes,
          "the cached estopped message is " + std::to_string(message.size()) + " bytes");
  h.core.feed(commandFrame("cmd_bigreason", "drive", "{\"throttle\":0.5}", h.now, h.now + 300));
  REQUIRE(h.lastNack("cmd_bigreason", fault, message) && message.size() <= ov::kMaxEstopReasonBytes,
          "the cached estopped message grew on a repeat");
  return "";
}

std::string scenarioValueOverflow(const std::string& hello, const std::string& config) {
  // The document a command's value is copied into can fail to allocate under memory pressure. The value that reaches
  // a driver must then be refused (bad_value), never a truncated document with a field missing. Only the value-document
  // seam is stressed (the frame parser and emit keep their own healthy allocator), so the frame still decodes and the
  // refusal can go out.
  const char* kinds[] = {"drive", "actuator"};
  const char* values[] = {"{\"throttle\":0.5,\"steer\":0.1}", "{\"name\":\"pan\",\"value\":0.5}"};
  for (int k = 0; k < 2; ++k) {
    const std::string id = std::string("cmd_overflow_") + kinds[k];
    const std::string frame = commandFrame(id, kinds[k], values[k], kT, kT + 300);
    H h;
    h.now = kT;
    h.core.connect();
    h.core.feed(hello);
    h.core.feed(config);
    h.sent.clear();
    h.value_alloc.fail = true; // every allocation of the value document fails
    h.core.feed(frame);
    h.value_alloc.fail = false;
    REQUIRE(!h.core.link_lost(), "the frame parser failed although only the value document was stressed");
    REQUIRE(h.drives.empty(), "a drive reached the driver while its value document could not be built");
    REQUIRE(h.actuator_names.empty(), "an actuator reached the driver while its value document could not be built");
    std::string fault;
    std::string message;
    REQUIRE(h.lastNack(id, fault, message), std::string("an overflowing ") + kinds[k] + " command was not refused");
    REQUIRE(fault == "bad_value", std::string("an overflowing ") + kinds[k] + " command nacked " + fault);
    // A repeat answers from the cache with the same refusal.
    h.value_alloc.fail = true;
    h.core.feed(frame);
    h.value_alloc.fail = false;
    REQUIRE(h.lastNack(id, fault, message) && fault == "bad_value", "the cached refusal changed");
    // With the allocator healthy the same command runs and reaches the driver whole.
    H ok;
    ok.now = kT;
    ok.core.connect();
    ok.core.feed(hello);
    ok.core.feed(config);
    ok.sent.clear();
    ok.core.feed(frame);
    if (k == 0) {
      REQUIRE(ok.drives.size() == 1, "the drive command did not run when the value allocator was healthy");
      JsonDocument d;
      REQUIRE(parse(ok.drives[0], d) && !d["throttle"].isNull(),
              "the healthy drive reached the driver without a throttle");
    } else {
      REQUIRE(ok.actuator_names.size() == 1 && ok.actuator_names[0] == "pan",
              "the actuator command did not run when the value allocator was healthy");
      JsonDocument d;
      REQUIRE(parse(ok.actuator_values[0], d) && std::string(d["name"] | "") == "pan",
              "the healthy actuator reached the driver without its name");
    }
  }
  return "";
}

std::string scenarioFixtures(const std::string& dir) {
  const char* names[] = {"ack",          "command_actuator", "command_drive", "config",     "error",
                         "estop",        "estop_state",      "heartbeat",     "heartbeat_ack", "heartbeat_ack_echo",
                         "hello",        "nack",             "paired",        "pair",       "status",
                         "telemetry"};
  std::map<std::string, std::string> fx;
  for (const char* n : names) {
    fx[n] = readFile(dir + "/" + n + ".json");
    if (fx[n].empty()) return std::string("fixture ") + n + ".json is missing or empty";
  }
  const std::string hello = fx["hello"];
  const std::string config = fx["config"];

  // Every server-to-device frame decodes and is handled (hello and config first, as on the wire).
  const char* server_frames[] = {"hello", "config", "command_drive", "command_actuator", "estop",
                                 "heartbeat_ack", "heartbeat_ack_echo", "error"};
  for (const char* n : server_frames) {
    H h;
    h.now = kT;
    h.core.connect();
    h.core.feed(hello);
    h.core.feed(config);
    h.core.feed(fx[n]);
    if (!h.core.link_up()) return std::string("feeding the ") + n + " fixture lost the link";
  }
  {
    H h;
    h.now = kT;
    h.core.connect();
    h.core.feed(hello);
    h.core.feed(config);
    h.core.feed(fx["command_drive"]);
    if (h.drives.size() != 1) return "the command_drive fixture did not reach on_drive";
    h.core.feed(fx["command_actuator"]);
    if (h.last_actuator_name != "pan") return "the command_actuator fixture did not reach on_actuator";
  }

  // The pairing fixtures: `paired` parses to the pairing answer, `pair` is built as the request body.
  ov::Paired paired;
  if (!ov::Core::parse_paired(fx["paired"], paired)) return "the paired fixture did not parse";
  if (paired.device_id != "dev_01J8Z4…" || paired.robot_id != "rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X") {
    return "the paired fixture parsed the wrong ids";
  }
  {
    const std::string req = ov::Core::build_pair_request("rob_01J8Z4M2Q0R7T9YV3K6N8P1W2X", "7Q2M4XZP", "0.1.0",
                                                         "onboard", "esp32", "{\"esp32\":{}}", "Rover");
    std::string why;
    if (!frameMatchesFixture(req, fx["pair"], why)) return "pair request: " + why;
  }

  // Every device-to-server frame the core emits carries the fixture's keys with its value types.
  std::string why;
  {
    H h;
    h.now = kT;
    h.core.connect();
    h.core.feed(hello);
    h.core.feed(config);
    JsonDocument d;
    if (!lastOfType(h.sent, "status", d)) return "no status emitted for the status fixture";
    std::string s;
    serializeJson(d, s);
    if (!frameMatchesFixture(s, fx["status"], why)) return "status: " + why;
    if (!lastOfType(h.sent, "estop_state", d)) return "no estop_state emitted";
    serializeJson(d, s);
    if (!frameMatchesFixture(s, fx["estop_state"], why)) return "estop_state: " + why;

    h.core.feed(commandFrame("cmd_ack", "drive", "{\"throttle\":0.1}", h.now, h.now + 300));
    if (!lastOfType(h.sent, "ack", d)) return "no ack emitted";
    serializeJson(d, s);
    if (!frameMatchesFixture(s, fx["ack"], why)) return "ack: " + why;

    h.core.feed(commandFrame("cmd_nack", "jump", "{}", h.now, h.now + 300));
    if (!lastOfType(h.sent, "nack", d)) return "no nack emitted";
    serializeJson(d, s);
    if (!frameMatchesFixture(s, fx["nack"], why)) return "nack: " + why;

    h.sent.clear();
    h.core.tick();
    if (!lastOfType(h.sent, "heartbeat", d)) return "no heartbeat emitted";
    const int64_t hb_t = d["t"].as<long long>();
    h.now += 50;
    h.core.feed("{\"v\":1,\"seq\":50,\"ts\":" + std::to_string(h.now) + ",\"type\":\"heartbeat_ack\",\"echo\":" +
                std::to_string(hb_t) + "}");
    h.sent.clear();
    h.now += 1000;
    h.core.tick();
    if (!lastOfType(h.sent, "heartbeat", d)) return "no measured heartbeat emitted";
    serializeJson(d, s);
    if (!frameMatchesFixture(s, fx["heartbeat"], why)) return "heartbeat: " + why;

    ov::Telemetry t;
    t.has_battery = true;
    t.battery = 0.72;
    t.has_voltage = true;
    t.voltage = 7.41;
    t.has_rssi = true;
    t.rssi = -57;
    t.sensors_json = "{\"ultrasonic\":118}";
    h.sent.clear();
    h.core.send_telemetry(t);
    if (!lastOfType(h.sent, "telemetry", d)) return "no telemetry emitted";
    serializeJson(d, s);
    if (!frameMatchesFixture(s, fx["telemetry"], why)) return "telemetry: " + why;
  }
  return "";
}

int failures = 0;

void report(const char* name, const std::string& problem) {
  if (problem.empty()) {
    std::cout << "PASS " << name << "\n";
  } else {
    std::cout << "FAIL " << name << " — " << problem << "\n";
    failures++;
  }
}

} // namespace

int main(int argc, char** argv) {
  const std::string dir = argc > 1 ? argv[1] : "internal/protocol/testdata/bot";
  const std::string hello = readFile(dir + "/hello.json");
  const std::string config = readFile(dir + "/config.json");
  const std::string drive_fixture = readFile(dir + "/command_drive.json");
  if (hello.empty() || config.empty() || drive_fixture.empty()) {
    std::cout << "FAIL fixtures — could not read hello/config/command_drive from " << dir << "\n";
    return 1;
  }

  report("fixtures replayed", scenarioFixtures(dir));
  report("hello on connect", scenarioHello(hello, config));
  report("heartbeat cadence and acks", scenarioHeartbeat(hello, config));
  report("deadman stops motion after the window, not before", scenarioDeadman(hello, config, drive_fixture));
  report("estop latches and clears", scenarioEstop(hello, config));
  report("clamping to the owner limits", scenarioClamp(hello));
  report("unknown kind and malformed value nack codes", scenarioNacks(hello, config));
  report("job refused on a device without the worker", scenarioJob(hello, config));
  report("close codes and reconnect policy", scenarioClose(hello, config));
  report("the wall clock stepping backwards leaves the safety timers alone", scenarioClock(hello, config));
  report("64-bit monotonic time across the 32-bit millis wrap", scenarioMonotonic64(hello, config));
  report("wrong-typed latched/estop_latched keep the latch", scenarioWrongTypedLatch(hello, config));
  report("oversize, too-deep and allocation-failing frames stop motion", scenarioFrameLimits(hello, config));
  report("long command ids and kinds are rejected", scenarioLongIds(hello, config));
  report("the dedup cache expires after ten minutes", scenarioDedupTtl(hello, config));
  report("heartbeat_ms is capped and the deadman cannot overflow", scenarioHeartbeatCap(hello));
  report("guarded commands are refused while the link is lost", scenarioLinkLostGuard(hello, config, drive_fixture));
  report("the reconnect backoff resets after a stable link", scenarioBackoffReset(hello));
  report("the 10 s floor after a refused reconnect", scenarioRefusedFloor(hello));
  report("the pre-hello timer does not fire with the transport down", scenarioPreHelloTimer());
  report("a huge e-stop by is bounded in every cached message", scenarioEstopReasonBound(hello, config));
  report("an overflowing command or actuator value is refused", scenarioValueOverflow(hello, config));

  if (failures > 0) {
    std::cout << failures << " scenario(s) failed\n";
    return 1;
  }
  std::cout << "all scenarios passed\n";
  return 0;
}
