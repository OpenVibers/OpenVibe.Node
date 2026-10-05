// conformance.cpp — the desktop conformance harness for ov_core.
//
// Builds with `g++ -std=c++17 -Wall -Wextra -Werror` against esp32/src/ov_core.cpp and the pinned ArduinoJson single
// header in esp32/test/.deps (run.sh fetches and checks it). It replays every fixture in
// internal/protocol/testdata/bot — each server-to-device frame is decoded and handled, and each device-to-server frame
// the core emits is checked for the fixture's keys and value types — and runs the protocol's scenarios (deadman,
// e-stop, clamping, nack codes, a job refusal, close codes, heartbeat cadence, hello/config ordering).
//
// Exit status is non-zero if any scenario fails; every scenario prints one PASS/FAIL line.
#include <ArduinoJson.h>

#include <cmath>
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

struct H {
  int64_t now = 0;
  std::vector<std::string> sent;
  std::vector<std::string> drives;
  std::vector<std::string> stops;
  std::vector<std::string> logs;
  std::string last_drive;
  std::string last_actuator_name;
  std::string last_actuator_value;
  int last_deadline = 0;
  ov::Core core;

  H()
      : core([this]() { return now; }, [this](const std::string& s) { sent.push_back(s); }) {
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

std::string configFrame(double max_speed, double max_turn, int max_command_ms, int heartbeat_ms) {
  return "{\"v\":1,\"seq\":1,\"ts\":1,\"type\":\"config\",\"heartbeat_ms\":" + std::to_string(heartbeat_ms) +
         ",\"limits\":{\"max_speed\":" + std::to_string(max_speed) + ",\"max_turn\":" + std::to_string(max_turn) +
         ",\"max_command_ms\":" + std::to_string(max_command_ms) +
         "},\"allowed_commands\":[\"drive\",\"actuator\",\"halt\"],\"estop_latched\":false}";
}

const int64_t kT = 1738065600000LL;

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

  if (failures > 0) {
    std::cout << failures << " scenario(s) failed\n";
    return 1;
  }
  std::cout << "all scenarios passed\n";
  return 0;
}
