// ov_core.cpp — see ov_core.h. Portable C++17, ArduinoJson v7 only. No Arduino headers.
#include "ov_core.h"

#include <algorithm>
#include <cmath>
#include <cstdio>
#include <cstring>

namespace ov {
namespace {

// RFC 3339 UTC from Unix milliseconds (protocol: estop_state.at, heartbeat server_time is the server's, not ours).
std::string iso8601_utc(int64_t unix_ms) {
  int64_t secs = unix_ms / 1000;
  int ms = static_cast<int>(unix_ms % 1000);
  if (ms < 0) {
    ms += 1000;
    secs -= 1;
  }
  int64_t days = secs / 86400;
  int64_t rem = secs % 86400;
  if (rem < 0) {
    rem += 86400;
    days -= 1;
  }
  const int hh = static_cast<int>(rem / 3600);
  const int mm = static_cast<int>((rem % 3600) / 60);
  const int ss = static_cast<int>(rem % 60);
  // Howard Hinnant's civil-from-days.
  int64_t z = days + 719468;
  const int64_t era = (z >= 0 ? z : z - 146096) / 146097;
  const int64_t doe = z - era * 146097;
  const int64_t yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
  int64_t y = yoe + era * 400;
  const int64_t doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
  const int64_t mp = (5 * doy + 2) / 153;
  const int64_t d = doy - (153 * mp + 2) / 5 + 1;
  const int64_t m = mp + (mp < 10 ? 3 : -9);
  y += (m <= 2);
  char buf[48];
  std::snprintf(buf, sizeof(buf), "%04d-%02d-%02dT%02d:%02d:%02d.%03dZ", static_cast<int>(y), static_cast<int>(m),
                static_cast<int>(d), hh, mm, ss, ms);
  return std::string(buf);
}

bool is_ulid26(const std::string& s) {
  if (s.size() != 26) return false;
  for (char c : s) {
    if (c >= '0' && c <= '9') continue;
    if (c >= 'A' && c <= 'Z') {
      if (c == 'I' || c == 'L' || c == 'O' || c == 'U') return false;
      continue;
    }
    return false;
  }
  return true;
}

bool valid_job_id(const std::string& s) {
  return s.size() == 30 && s.compare(0, 4, "job_") == 0 && is_ulid26(s.substr(4));
}

bool valid_media_id(const std::string& s) {
  return s.size() == 30 && s.compare(0, 4, "med_") == 0 && is_ulid26(s.substr(4));
}

bool is_lower_alnum(char c) { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'); }

bool artifact_name_ok(const std::string& s) {
  if (s.empty() || s.size() > 128 || !is_lower_alnum(s[0])) return false;
  for (char c : s) {
    if (is_lower_alnum(c) || c == '.' || c == '_' || c == '-') continue;
    return false;
  }
  return true;
}

bool is_alnum(char c) { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'); }

bool artifact_version_ok(const std::string& s) {
  if (s.empty() || s.size() > 64 || !is_alnum(s[0])) return false;
  for (char c : s) {
    if (is_alnum(c) || c == '.' || c == '+' || c == '-') continue;
    return false;
  }
  return true;
}

bool input_name_ok(const std::string& s) {
  if (s.empty() || s.size() > 128 || !is_alnum(s[0])) return false;
  for (char c : s) {
    if (is_alnum(c) || c == '.' || c == '_' || c == '-') continue;
    return false;
  }
  return true;
}

bool sha256_ok(const std::string& s) {
  if (s.size() != 64) return false;
  for (char c : s) {
    if ((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) continue;
    return false;
  }
  return true;
}

bool is_runtime_class(const std::string& c) {
  return c == "function" || c == "code" || c == "browser" || c == "linux" || c == "desktop" || c == "gpu";
}

bool is_integral(JsonVariantConst v) { return v.is<long long>(); }

// A field present with the wrong JSON type is a malformed job (Go's json.Unmarshal into the Job struct errors).
bool job_types_ok(JsonObjectConst j) {
  JsonVariantConst cls = j["class"];
  if (!cls.isNull() && !cls.is<const char*>()) return false;
  JsonVariantConst art = j["artifact"];
  if (!art.isNull()) {
    if (!art.is<JsonObjectConst>()) return false;
    JsonVariantConst n = art["name"];
    if (!n.isNull() && !n.is<const char*>()) return false;
    JsonVariantConst v = art["version"];
    if (!v.isNull() && !v.is<const char*>()) return false;
  }
  JsonVariantConst ttl = j["ttl_ms"];
  if (!ttl.isNull() && !is_integral(ttl)) return false;
  JsonVariantConst lim = j["limits"];
  if (!lim.isNull()) {
    if (!lim.is<JsonObjectConst>()) return false;
    JsonObjectConst lo = lim.as<JsonObjectConst>();
    for (const char* k : {"wall_ms", "cpu_ms", "mem_bytes"}) {
      JsonVariantConst v = lo[k];
      if (!v.isNull() && !is_integral(v)) return false;
    }
  }
  JsonVariantConst net = j["net"];
  if (!net.isNull() && !net.is<const char*>()) return false;
  JsonVariantConst in = j["inputs"];
  if (!in.isNull()) {
    if (!in.is<JsonArrayConst>()) return false;
    for (JsonVariantConst e : in.as<JsonArrayConst>()) {
      if (!e.is<JsonObjectConst>()) return false;
      JsonObjectConst eo = e.as<JsonObjectConst>();
      for (const char* k : {"name", "media_id", "sha256"}) {
        JsonVariantConst v = eo[k];
        if (!v.isNull() && !v.is<const char*>()) return false;
      }
      JsonVariantConst sz = eo["size_bytes"];
      if (!sz.isNull() && !is_integral(sz)) return false;
    }
  }
  return true;
}

bool valid_job_inputs(JsonObjectConst j) {
  JsonVariantConst in = j["inputs"];
  if (in.isNull()) return true;
  if (!in.is<JsonArrayConst>()) return false;
  JsonArrayConst arr = in.as<JsonArrayConst>();
  if (arr.size() > 32) return false;
  std::vector<std::string> names;
  for (JsonVariantConst e : arr) {
    if (!e.is<JsonObjectConst>()) return false;
    JsonObjectConst eo = e.as<JsonObjectConst>();
    std::string name = eo["name"] | "";
    std::string media = eo["media_id"] | "";
    std::string sha = eo["sha256"] | "";
    long long size = eo["size_bytes"] | 0LL;
    if (!input_name_ok(name) || !valid_media_id(media) || !sha256_ok(sha) || size < 0) return false;
    if (std::find(names.begin(), names.end(), name) != names.end()) return false;
    names.push_back(name);
  }
  return true;
}

} // namespace

Core::Core(NowFn now, WallFn wall, SendFn send)
    : now_(std::move(now)), wall_(std::move(wall)), send_(std::move(send)) {}

int64_t Core::wall_ms() const { return wall_ ? wall_() : now_(); }

void Core::set_callbacks(DriveFn drive, ActuatorFn actuator, StopFn stop, LogFn log) {
  drive_ = std::move(drive);
  actuator_ = std::move(actuator);
  stop_ = std::move(stop);
  log_fn_ = std::move(log);
}

void Core::set_latch_callback(LatchFn latch) { latch_fn_ = std::move(latch); }

void Core::set_local_limits(double max_speed, double max_turn, int max_command_ms) {
  local_limits_.max_speed = std::max(0.0, std::min(1.0, max_speed));
  local_limits_.max_turn = std::max(0.0, std::min(1.0, max_turn));
  local_limits_.max_command_ms = max_command_ms > 0 ? max_command_ms : kDefaultMaxCommandMS;
}

void Core::set_descriptor(const std::string& firmware, const std::string& driver,
                          const std::string& capabilities_json) {
  firmware_ = firmware;
  driver_ = driver;
  capabilities_json_ = capabilities_json.empty() ? "{}" : capabilities_json;
}

void Core::log(const char* level, const std::string& message) const {
  if (log_fn_) log_fn_(level, message);
}

void Core::notify_latch() const {
  if (latch_fn_) latch_fn_(remote_stop_, local_stop_);
}

void Core::emit(const char* type, const std::function<void(JsonObject)>& fill) {
  JsonDocument doc(json_allocator_ ? json_allocator_ : ArduinoJson::detail::DefaultAllocator::instance());
  doc["v"] = kVersion;
  doc["seq"] = ++seq_;
  doc["ts"] = wall_ms();
  doc["type"] = type;
  fill(doc.as<JsonObject>());
  if (doc.overflowed()) {
    // An ack/nack without its id (or any frame that lost fields to a failed allocation) must not go out half-formed.
    log("error", "could not build a frame: out of memory; dropping it");
    return;
  }
  std::string out;
  serializeJson(doc, out);
  if (send_) send_(out);
}

// ---- connection lifecycle ----

void Core::connect() {
  // A new connection: per-direction seq restarts at 1, the clock estimate and heartbeat state start empty, and no
  // config has been applied yet (so only halt runs, and the first status/estop_state wait for config).
  seq_ = 0;
  up_ = false;
  configured_ = false;
  allowed_present_ = false;
  allowed_.clear();
  has_skew_ = false;
  skew_ms_ = 0;
  has_rtt_ = false;
  last_rtt_ms_ = 0;
  hb_sent_.clear();
  credential_refused_ = false;
  link_lost_ = false;
  motion_deadline_ms_ = 0;
  device_id_.clear();
  session_id_.clear();
  robot_ids_.clear();
  hb_ms_ = kDefaultHeartbeatMS;
  deadman_ms_ = 2 * kDefaultHeartbeatMS;
  const int64_t now = now_();
  last_rx_ms_ = now;
  next_hb_ms_ = now;
  connected_at_ms_ = 0;
}

void Core::on_close(int code) {
  // Every close stops every actuator at once (protocol §Heartbeat, deadman, reconnect and §3).
  const bool was_up = up_;
  stop_motion();
  up_ = false;
  configured_ = false;
  if (code == kCloseInvalid || code == kCloseRevoked || (code == 0 && !was_up)) {
    // A drop before hello is assumed to be the refused case (the arduinoWebSockets library does not surface the close
    // code or the upgrade's HTTP status); the log says the cause is uncertain rather than asserting a refusal.
    credential_refused_ = true;
    if (code == 0) {
      log("warn", "the link ended before hello (the transport cannot know the close code: a refused credential, a "
                  "network drop or a timeout); retrying no sooner than 10 s");
    } else {
      log("error", "the server refused this device's credential: pair again, or import the owner's rotation");
    }
    const int ceil = kBackoffMaxMS;
    next_delay_ms_ = kCredentialRetryMinMS + static_cast<int>(next_random() % static_cast<uint32_t>(ceil + 1));
  } else {
    credential_refused_ = false;
    if (code == kCloseReplaced) {
      log("warn", "another connection with this credential replaced this one");
    }
    next_delay_ms_ = normal_backoff_ms();
  }
  reconnect_attempt_ += 1;
}

int Core::normal_backoff_ms() {
  // Exponential from BackoffMin to BackoffMax with full jitter; the floor keeps a flapping server from being hammered.
  int ceil = kBackoffMaxMS;
  if (reconnect_attempt_ < 32) {
    const long long c = static_cast<long long>(kBackoffMinMS) << reconnect_attempt_;
    if (c > 0 && c < ceil) ceil = static_cast<int>(c);
  }
  const int jitter = static_cast<int>(next_random() % static_cast<uint32_t>(ceil + 1));
  return kBackoffMinMS / 2 + jitter;
}

uint32_t Core::next_random() {
  uint32_t x = rng_state_;
  x ^= x << 13;
  x ^= x >> 17;
  x ^= x << 5;
  rng_state_ = x;
  return x;
}

// ---- server frames ----

void Core::feed(const char* data, size_t length) {
  const int64_t now = now_();
  if (length > kMaxFrameBytes) {
    // Too big to be any frame this device handles; an estop could be hiding in it, so treat it as unreadable.
    log("error", "frame of " + std::to_string(length) + " bytes is over the " + std::to_string(kMaxFrameBytes) +
                     "-byte device limit; stopping and dropping the link");
    stop_motion();
    link_lost_ = true;
    return;
  }
  JsonDocument doc(json_allocator_ ? json_allocator_ : ArduinoJson::detail::DefaultAllocator::instance());
  const DeserializationError err =
      deserializeJson(doc, data, length, DeserializationOption::NestingLimit(kMaxJsonDepth));
  if (err) {
    if (err == DeserializationError::NoMemory || err == DeserializationError::TooDeep) {
      // The frame could not be parsed for lack of memory or depth: it may have been an estop, so fail safe by stopping
      // and dropping the link; the reconnect re-reads config.estop_latched.
      log("error", std::string("frame could not be decoded safely (") + err.c_str() +
                       "); stopping and dropping the link");
      stop_motion();
      link_lost_ = true;
    } else {
      log("warn", std::string("bad frame: ") + err.c_str());
    }
    return;
  }
  const int v = doc["v"] | -1;
  if (v != kVersion) {
    log("warn", "bad frame: unsupported envelope version");
    return;
  }
  const char* type_c = doc["type"] | "";
  if (type_c == nullptr || *type_c == '\0') {
    log("warn", "bad frame: no type");
    return;
  }
  const std::string type(type_c);
  last_rx_ms_ = now;
  const int64_t ts = doc["ts"] | static_cast<int64_t>(0);
  if (ts > 0) observe(ts, now);

  if (type == "hello") {
    handle_hello(doc, now);
    return;
  }
  if (!up_) {
    log("warn", "ignoring a frame before hello: " + type);
    return;
  }
  if (type == "config") {
    handle_config(doc);
  } else if (type == "command") {
    handle_command(doc, ts, now);
  } else if (type == "estop") {
    handle_estop(doc);
  } else if (type == "heartbeat_ack") {
    handle_heartbeat_ack(doc, now);
  } else if (type == "error") {
    const std::string code = doc["code"] | "";
    const std::string detail = doc["detail"] | "";
    log("warn", "server refused a frame: " + code + " " + detail);
  } else if (type == "job") {
    handle_job(doc);
  } else if (type == "job_cancel" || type == "job_exit_ack") {
    // No worker runs jobs here, so an unknown, refused or ended id is ignored with no answer.
  } else {
    log("debug", "ignoring unknown frame type " + type);
  }
}

void Core::handle_hello(JsonDocument& doc, int64_t now) {
  if (up_) {
    log("debug", "ignoring a second hello on this connection");
    return;
  }
  up_ = true;
  device_id_ = doc["device_id"] | "";
  session_id_ = doc["session_id"] | "";
  robot_ids_.clear();
  JsonArrayConst ids = doc["robot_ids"];
  for (JsonVariantConst r : ids) {
    if (r.is<const char*>()) robot_ids_.emplace_back(r.as<const char*>());
  }
  last_rx_ms_ = now;
  next_hb_ms_ = now; // the first heartbeat goes out promptly once hello is up
  connected_at_ms_ = now;
  log("info", "link up: device " + device_id_ + ", session " + session_id_);
}

void Core::handle_config(JsonDocument& doc) {
  // A wrong-typed estop_latched is a malformed frame (Go's json.Unmarshal would reject it whole): apply nothing and
  // keep the latch exactly as it is, rather than reading `"false"` or `0` as false and releasing the e-stop.
  JsonVariantConst estopv = doc["estop_latched"];
  if (!estopv.isNull() && !estopv.is<bool>()) {
    log("warn", "config with a non-boolean estop_latched ignored; the latch is unchanged");
    return;
  }

  int hb = doc["heartbeat_ms"] | 0;
  if (hb <= 0) hb = doc["limits"]["heartbeat_ms"] | 0;
  if (hb <= 0) hb = kDefaultHeartbeatMS;
  if (hb > kMaxHeartbeatMS) {
    log("warn", "config.heartbeat_ms " + std::to_string(hb) + " capped at " + std::to_string(kMaxHeartbeatMS) + " ms");
    hb = kMaxHeartbeatMS;
  }
  hb_ms_ = hb;
  deadman_ms_ = 2 * static_cast<int64_t>(hb); // 64-bit: no overflow however the server sets hb

  double server_speed = 1.0;
  double server_turn = 1.0;
  int server_max_ms = kDefaultMaxCommandMS;
  JsonVariantConst lim = doc["limits"];
  if (lim.is<JsonObjectConst>()) {
    JsonVariantConst ms = lim["max_speed"];
    if (ms.is<double>()) server_speed = ms.as<double>();
    JsonVariantConst mt = lim["max_turn"];
    if (mt.is<double>()) server_turn = mt.as<double>();
    const int v = lim["max_command_ms"] | 0;
    if (v > 0) server_max_ms = v;
  }
  // The stricter of the server's limits and this device's local caps (protocol step 6).
  limits_.max_speed = std::max(0.0, std::min(std::min(1.0, server_speed), local_limits_.max_speed));
  limits_.max_turn = std::max(0.0, std::min(std::min(1.0, server_turn), local_limits_.max_turn));
  limits_.max_command_ms = server_max_ms;
  if (local_limits_.max_command_ms > 0 && local_limits_.max_command_ms < limits_.max_command_ms) {
    limits_.max_command_ms = local_limits_.max_command_ms;
  }
  if (limits_.max_command_ms <= 0) limits_.max_command_ms = kDefaultMaxCommandMS;

  allowed_present_ = false;
  allowed_.clear();
  JsonVariantConst ac = doc["allowed_commands"];
  if (ac.is<JsonArrayConst>()) {
    allowed_present_ = true;
    allowed_[kKindHalt] = true; // halt is always accepted
    for (JsonVariantConst k : ac.as<JsonArrayConst>()) {
      if (k.is<const char*>()) allowed_[std::string(k.as<const char*>())] = true;
    }
  }

  // The server's estop_latched is the owner's latch, which may have been set or cleared while the link was down.
  const bool estop = estopv.is<bool>() && estopv.as<bool>();
  if (estop && !remote_stop_) {
    set_remote_stop(true, "latched on the server", true);
  } else if (!estop && remote_stop_) {
    set_remote_stop(false, "", true);
  }

  const bool first = !configured_;
  configured_ = true;
  if (first) send_status();
  send_estop_state();
  log("info", "config applied: heartbeat " + std::to_string(hb_ms_) + " ms");
}

void Core::handle_command(JsonDocument& doc, int64_t ts, int64_t now) {
  // Bound the id before copying it: an id longer than kMaxIdBytes is not echoed (it is not cached either), so a
  // hostile server cannot make the dedup cache hold kilobyte ids.
  JsonVariantConst idv = doc["id"];
  if (!idv.is<const char*>() || *idv.as<const char*>() == '\0') {
    log("warn", "command without id ignored");
    return;
  }
  const size_t id_len = std::strlen(idv.as<const char*>());
  if (id_len > kMaxIdBytes) {
    log("warn", "command id longer than " + std::to_string(kMaxIdBytes) + " bytes ignored");
    return;
  }
  const std::string id(idv.as<const char*>(), id_len);
  DedupResult cached;
  if (recall(id, cached, now)) {
    if (cached.ok) {
      send_ack(id, 0);
    } else {
      send_nack(id, cached.fault.c_str(), cached.message);
    }
    return;
  }
  // The kind is bounded too; it is never echoed into a cached message (a fixed message keeps the cache's size fixed).
  JsonVariantConst kindv = doc["kind"];
  const std::string kind = kindv.is<const char*>() ? std::string(kindv.as<const char*>()) : std::string();
  const bool known = kind == kKindDrive || kind == kKindActuator || kind == kKindPTZ || kind == kKindSay ||
                     kind == kKindDisplay || kind == kKindHalt;
  if (!known || kind.size() > kMaxKindBytes) {
    send_nack(id, kFaultUnsupported, "unknown kind");
    remember(id, DedupResult{false, kFaultUnsupported, "unknown kind"});
    return;
  }
  if (kind == kKindHalt) {
    stop_motion();
    send_ack(id, now_() - now);
    remember(id, DedupResult{true, "", ""});
    return;
  }
  if (!configured_) {
    send_nack(id, kFaultNotReady, "the server's config has not arrived on this connection yet");
    remember(id, DedupResult{false, kFaultNotReady, "the server's config has not arrived on this connection yet"});
    return;
  }
  if (allowed_present_ && allowed_.find(kind) == allowed_.end()) {
    const std::string msg = kind + " is not in this robot's allowed_commands";
    send_nack(id, kFaultNotAllowed, msg);
    remember(id, DedupResult{false, kFaultNotAllowed, msg});
    return;
  }
  const bool guarded = kind == kKindDrive || kind == kKindPTZ || kind == kKindActuator;
  if (guarded) {
    if (link_lost_) {
      // The core has decided to drop the link but the transport has not torn the socket down yet: a guarded command
      // arriving in that window is not safe to run.
      const std::string msg = "the control link is down";
      send_nack(id, kFaultNotConnected, msg);
      remember(id, DedupResult{false, kFaultNotConnected, msg});
      return;
    }
    if (local_stop_) {
      const std::string msg = "stopped on the device with the local kill switch";
      send_nack(id, kFaultLocalStop, msg);
      remember(id, DedupResult{false, kFaultLocalStop, msg});
      return;
    }
    if (remote_stop_) {
      const std::string msg = remote_reason_.empty() ? std::string("the remote e-stop is latched") : remote_reason_;
      send_nack(id, kFaultEstopped, msg);
      remember(id, DedupResult{false, kFaultEstopped, msg});
      return;
    }
  }

  const int64_t deadline_ms = doc["deadline_ms"] | static_cast<int64_t>(0);
  int deadline_rel = 0;
  if (deadline_ms > 0) {
    const int64_t left = deadline_ms - server_now(ts, now);
    if (left <= 0) {
      const std::string msg = "the deadline passed " + std::to_string(-left) + " ms before the command arrived";
      send_nack(id, kFaultExpired, msg);
      remember(id, DedupResult{false, kFaultExpired, msg});
      return;
    }
    deadline_rel = static_cast<int>(std::min<int64_t>(left, limits_.max_command_ms));
  } else {
    deadline_rel = std::min(kDefaultDeadlineMS, limits_.max_command_ms);
  }
  if (deadline_rel <= 0) deadline_rel = kDefaultDeadlineMS;

  std::string value_json;
  std::string fault;
  std::string message;
  if (!clamp_command(kind, doc["value"], value_json, fault, message)) {
    send_nack(id, fault.c_str(), message);
    remember(id, DedupResult{false, fault, message});
    return;
  }

  if (kind == kKindDrive) {
    if (!drive_) {
      send_nack(id, kFaultUnsupported, "no driver on this device handles drive");
      remember(id, DedupResult{false, kFaultUnsupported, "no driver on this device handles drive"});
      return;
    }
    drive_(value_json, deadline_rel);
    motion_deadline_ms_ = now_() + deadline_rel;
    send_ack(id, now_() - now);
    remember(id, DedupResult{true, "", ""});
    return;
  }
  if (kind == kKindActuator) {
    JsonDocument vd;
    deserializeJson(vd, value_json);
    const std::string name = vd["name"] | "";
    if (!actuator_) {
      send_nack(id, kFaultUnsupported, "no driver on this device handles actuator");
      remember(id, DedupResult{false, kFaultUnsupported, "no driver on this device handles actuator"});
      return;
    }
    actuator_(name, value_json, deadline_rel);
    motion_deadline_ms_ = now_() + deadline_rel;
    send_ack(id, now_() - now);
    remember(id, DedupResult{true, "", ""});
    return;
  }
  // ptz, say and display are valid kinds with no driver on this device (protocol step 7).
  const std::string msg = "no driver on this device handles " + kind;
  send_nack(id, kFaultUnsupported, msg);
  remember(id, DedupResult{false, kFaultUnsupported, msg});
}

void Core::handle_estop(JsonDocument& doc) {
  JsonVariantConst latchedv = doc["latched"];
  if (!latchedv.isNull() && !latchedv.is<bool>()) {
    // Wrong type: ignore the frame and keep the latch (Go's json.Unmarshal rejects it too). Reading `"false"` or 0 as
    // false would release the e-stop on a malformed frame.
    log("warn", "estop with a non-boolean latched ignored; the latch is unchanged");
    return;
  }
  const bool latched = latchedv.is<bool>() && latchedv.as<bool>();
  if (latched) {
    const std::string by = doc["by"] | "";
    set_remote_stop(true, by.empty() ? std::string("the remote e-stop is latched") : ("by " + by), true);
  } else {
    set_remote_stop(false, "", true);
  }
  send_estop_state();
}

void Core::handle_heartbeat_ack(JsonDocument& doc, int64_t now) {
  // Both shapes the fixtures carry: the newer echo/t fields, and an older server that echoes the heartbeat's seq.
  int64_t echo = doc["t"] | static_cast<int64_t>(0);
  if (echo == 0) {
    JsonVariantConst e = doc["echo"];
    if (e.is<long long>()) echo = e.as<long long>();
  }
  const uint64_t seq = doc["seq"] | static_cast<uint64_t>(0);
  bool found = false;
  int64_t at = 0;
  if (echo > 0) {
    for (auto it = hb_sent_.begin(); it != hb_sent_.end(); ++it) {
      if (it->second.t == echo) {
        at = it->second.at;
        hb_sent_.erase(it);
        found = true;
        break;
      }
    }
  }
  if (!found && seq != 0) {
    auto it = hb_sent_.find(seq);
    if (it != hb_sent_.end()) {
      at = it->second.at;
      hb_sent_.erase(it);
      found = true;
    }
  }
  if (found) {
    last_rtt_ms_ = now > at ? now - at : 0; // a measured 0 ms is still a measurement
    has_rtt_ = true;
  }
}

void Core::handle_job(JsonDocument& doc) {
  JsonVariantConst jobv = doc["job"];
  if (!jobv.is<JsonObjectConst>()) {
    // Without a `job` object Go cannot reach an id at all, so the nack carries an empty one.
    send_nack("", kFaultBadValue, kJobBadID);
    return;
  }
  JsonObjectConst j = jobv.as<JsonObjectConst>();
  JsonVariantConst idv = j["id"];
  if (!idv.is<const char*>()) {
    send_nack("", kFaultBadValue, kJobBadID);
    return;
  }
  const std::string id = idv.as<const char*>();
  if (!valid_job_id(id)) {
    send_nack(id, kFaultBadValue, kJobBadID);
    return;
  }
  const auto seen = job_answers_.find(id);
  if (seen != job_answers_.end()) {
    send_nack(id, seen->second.first.c_str(), seen->second.second);
    return;
  }
  if (!job_types_ok(j)) {
    send_nack(id, kFaultBadFrame, kJobBadFrame);
    remember_job(id, kFaultBadFrame, kJobBadFrame);
    return;
  }
  const std::string cls = j["class"] | "";
  if (!is_runtime_class(cls)) {
    send_nack(id, kFaultUnsupported, kJobUnknownClass);
    remember_job(id, kFaultUnsupported, kJobUnknownClass);
    return;
  }
  if (cls == "function" || cls == "code") {
    JsonVariantConst art = j["artifact"];
    bool ok = art.is<JsonObjectConst>();
    if (ok) {
      JsonObjectConst ao = art.as<JsonObjectConst>();
      const std::string an = ao["name"] | "";
      const std::string av = ao["version"] | "";
      ok = artifact_name_ok(an) && artifact_version_ok(av);
    }
    if (!ok) {
      send_nack(id, kFaultBadValue, kJobNoArtifact);
      remember_job(id, kFaultBadValue, kJobNoArtifact);
      return;
    }
  }
  JsonVariantConst netv = j["net"];
  if (netv.is<const char*>()) {
    const std::string net = netv.as<const char*>();
    if (!(net == "deny" || net == "none" || net == "public" || net == "openvibe-only")) {
      send_nack(id, kFaultUnsupported, kJobNetUnsupported);
      remember_job(id, kFaultUnsupported, kJobNetUnsupported);
      return;
    }
  }
  if (!valid_job_inputs(j)) {
    send_nack(id, kFaultBadValue, kJobBadInputs);
    remember_job(id, kFaultBadValue, kJobBadInputs);
    return;
  }
  JsonVariantConst args = j["args"];
  JsonObjectConst lo = j["limits"].as<JsonObjectConst>();
  const long long ttl = j["ttl_ms"] | 0LL;
  const long long wall = lo["wall_ms"] | 0LL;
  const long long cpu = lo["cpu_ms"] | 0LL;
  const long long mem = lo["mem_bytes"] | 0LL;
  if (!args.is<JsonObjectConst>() || ttl < 1 || wall < 1 || cpu < 1 || mem < 1) {
    send_nack(id, kFaultBadValue, kJobBadLimits);
    remember_job(id, kFaultBadValue, kJobBadLimits);
    return;
  }
  // No worker is on this device, so every valid class is unavailable (protocol §Jobs, `class not available`).
  send_nack(id, kFaultUnsupported, kJobNotAvailable);
  remember_job(id, kFaultUnsupported, kJobNotAvailable);
}

// ---- time, clamping, routing helpers ----

void Core::observe(int64_t ts, int64_t now) {
  // The skew is estimated against the monotonic clock: the server's Unix ts advances at the same rate, and a wall-clock
  // step (NTP, a backwards correction) cannot poison the estimate and open an expired deadline.
  if (ts <= 0) return;
  if (!has_skew_) {
    has_skew_ = true;
    skew_ms_ = ts - now;
    return;
  }
  const int64_t s = ts - now;
  if (s > skew_ms_) skew_ms_ = s; // the largest ts − receive time: a delayed frame cannot reopen an expired deadline
}

int64_t Core::server_now(int64_t ts, int64_t now) const {
  if (has_skew_) return now + skew_ms_;
  if (ts > 0) return ts;
  return now;
}

bool Core::clamp_command(const std::string& kind, JsonVariantConst value, std::string& out, std::string& fault,
                         std::string& message) const {
  JsonDocument vd;
  if (!value.isNull()) {
    if (!value.is<JsonObjectConst>()) {
      fault = kFaultBadValue;
      message = "value must be a JSON object";
      return false;
    }
    vd.set(value);
  }
  if (!vd.is<JsonObject>()) vd.to<JsonObject>();
  JsonObject v = vd.as<JsonObject>();

  auto num = [&](const char* k, JsonVariantConst raw, double& result) -> bool {
    if (!raw.is<double>()) {
      fault = kFaultBadValue;
      message = std::string(k) + " must be a number";
      return false;
    }
    const double f = raw.as<double>();
    if (std::isnan(f) || std::isinf(f)) {
      fault = kFaultBadValue;
      message = std::string(k) + " must be a finite number";
      return false;
    }
    result = f;
    return true;
  };
  auto clamp_to = [](double f, double m) {
    if (f > m) return m;
    if (f < -m) return -m;
    return f;
  };

  if (kind == kKindDrive) {
    for (JsonPair kv : v) {
      const std::string key = kv.key().c_str();
      const bool speed = key == "throttle" || key == "x" || key == "y";
      const bool turn = key == "steer" || key == "rotation";
      if (!speed && !turn) continue;
      double f = 0;
      if (!num(key.c_str(), kv.value(), f)) return false;
      const double m = speed ? limits_.max_speed : limits_.max_turn;
      kv.value().set(clamp_to(clamp_to(f, 1.0), m));
    }
  } else if (kind == kKindPTZ) {
    for (JsonPair kv : v) {
      const std::string key = kv.key().c_str();
      if (key != "pan" && key != "tilt" && key != "zoom") continue;
      double f = 0;
      if (!num(key.c_str(), kv.value(), f)) return false;
      kv.value().set(clamp_to(f, 1.0));
    }
  } else if (kind == kKindActuator) {
    JsonVariantConst name = v["name"];
    if (!name.is<const char*>() || std::string(name.as<const char*>()).empty()) {
      fault = kFaultBadValue;
      message = "actuator needs a name";
      return false;
    }
    JsonVariant val = v["value"];
    if (val.is<double>()) {
      const double f = val.as<double>();
      if (std::isnan(f) || std::isinf(f)) {
        fault = kFaultBadValue;
        message = "value must be a finite number";
        return false;
      }
      val.set(clamp_to(f, 1.0));
    }
  } else if (kind == kKindSay) {
    JsonVariantConst t = v["text"];
    if (!t.is<const char*>()) {
      fault = kFaultBadValue;
      message = "say needs text (1 to 1000 bytes)";
      return false;
    }
    const std::string s = t.as<const char*>();
    if (s.empty() || s.size() > 1000) {
      fault = kFaultBadValue;
      message = "say needs text (1 to 1000 bytes)";
      return false;
    }
  }
  serializeJson(vd, out);
  return true;
}

void Core::send_ack(const std::string& id, int64_t latency_ms) {
  emit("ack", [&](JsonObject o) {
    o["id"] = id;
    if (latency_ms > 0) o["latency_ms"] = latency_ms;
  });
}

void Core::send_nack(const std::string& id, const char* fault, const std::string& message) {
  emit("nack", [&](JsonObject o) {
    o["id"] = id;
    o["fault_code"] = fault;
    if (!message.empty()) o["message"] = message;
  });
}

void Core::remember(const std::string& id, DedupResult result) {
  if (dedup_.find(id) == dedup_.end()) dedup_order_.push_back(id);
  result.at_ms = now_();
  dedup_[id] = std::move(result);
  while (dedup_order_.size() > 256) {
    dedup_.erase(dedup_order_.front());
    dedup_order_.erase(dedup_order_.begin());
  }
}

bool Core::recall(const std::string& id, DedupResult& result, int64_t now) {
  const auto it = dedup_.find(id);
  if (it == dedup_.end()) return false;
  if (now - it->second.at_ms > kDedupTTLMS) {
    // The spec's ten minutes have passed: forget it, so the id runs again rather than being answered from a stale entry.
    dedup_.erase(it);
    dedup_order_.erase(std::remove(dedup_order_.begin(), dedup_order_.end(), id), dedup_order_.end());
    return false;
  }
  result = it->second;
  return true;
}

void Core::remember_job(const std::string& id, const char* fault, const std::string& message) {
  if (job_answers_.find(id) == job_answers_.end()) job_order_.push_back(id);
  job_answers_[id] = {fault, message};
  while (job_order_.size() > 64) {
    job_answers_.erase(job_order_.front());
    job_order_.erase(job_order_.begin());
  }
}

void Core::stop_motion() {
  motion_deadline_ms_ = 0;
  if (stop_) stop_();
}

void Core::set_remote_stop(bool on, const std::string& reason, bool notify) {
  if (on) {
    remote_stop_ = true;
    remote_reason_ = reason;
    stop_motion();
    if (notify) {
      log("warn", "e-stop latched: " + reason);
      notify_latch();
    }
  } else {
    remote_stop_ = false;
    remote_reason_.clear();
    if (notify) {
      log("warn", "e-stop cleared; motion allowed again");
      notify_latch();
    }
  }
}

void Core::set_local_stop(bool on) {
  if (on == local_stop_) return;
  local_stop_ = on;
  if (on) {
    stop_motion();
    log("warn", "local kill switch latched");
  } else {
    log("warn", "local kill switch cleared; motion allowed again");
  }
  notify_latch();
  send_estop_state();
}

// ---- heartbeat cadence, deadman, motion deadline ----

void Core::tick() {
  const int64_t now = now_();
  // A session that has been up for kStableLinkMS resets the reconnect backoff, exactly as the Go Node's Run does after
  // a session of more than 30 s; otherwise a handful of drops would leave the delay at its maximum forever.
  if (up_ && reconnect_attempt_ > 0 && connected_at_ms_ > 0 && now - connected_at_ms_ >= kStableLinkMS) {
    reconnect_attempt_ = 0;
  }
  if (!up_) {
    const int64_t allowance = std::max<int64_t>(deadman_ms_, kHelloAllowanceMS);
    if (!link_lost_ && now - last_rx_ms_ > allowance) {
      link_lost_ = true;
      stop_motion();
      log("error", "no hello from the server in time; closing the link");
    }
    return;
  }
  if (now >= next_hb_ms_) {
    send_heartbeat();
    next_hb_ms_ = now + hb_ms_;
  }
  if (!link_lost_ && now - last_rx_ms_ > deadman_ms_) {
    link_lost_ = true;
    stop_motion();
    log("error", "heartbeat lost: nothing from the server in time; closing the link");
    return;
  }
  if (motion_deadline_ms_ > 0 && now >= motion_deadline_ms_) {
    stop_motion();
    log("warn", "the motion deadline passed; stopping");
  }
}

void Core::send_heartbeat() {
  const int64_t t = wall_ms(); // protocol §Device → server: the heartbeat's send time in Unix ms
  const int64_t at = now_();
  emit("heartbeat", [&](JsonObject o) {
    o["seq"] = seq_; // the envelope's seq, as older servers echo
    o["t"] = t;
    if (has_rtt_) o["rtt_ms"] = last_rtt_ms_;
  });
  hb_sent_[seq_] = HbSend{t, at};
  for (auto it = hb_sent_.begin(); it != hb_sent_.end();) {
    if (it->first + 8 < seq_) {
      it = hb_sent_.erase(it);
    } else {
      ++it;
    }
  }
}

// ---- status, estop_state, telemetry ----

void Core::send_status() {
  if (!configured_) return;
  emit("status", [&](JsonObject o) {
    o["firmware"] = firmware_;
    JsonDocument caps;
    if (deserializeJson(caps, capabilities_json_)) {
      caps.to<JsonObject>();
    }
    o["capabilities"].set(caps.as<JsonVariantConst>());
    o["faults"].to<JsonArray>();
    o["estop_latched"] = estopped();
  });
}

void Core::send_estop_state() {
  if (!configured_) return;
  emit("estop_state", [&](JsonObject o) {
    o["latched"] = estopped();
    o["by"] = "device";
    o["at"] = iso8601_utc(wall_ms());
    if (robot_ids_.size() == 1) o["robot_id"] = robot_ids_[0];
    o["local_stop"] = local_stop_;
    if (!remote_reason_.empty()) o["reason"] = remote_reason_;
  });
}

void Core::send_telemetry(const Telemetry& telemetry) {
  emit("telemetry", [&](JsonObject o) {
    if (telemetry.has_battery) o["battery"] = telemetry.battery;
    if (telemetry.has_voltage) o["voltage"] = telemetry.voltage;
    if (telemetry.has_rssi) o["rssi"] = telemetry.rssi;
    if (!telemetry.sensors_json.empty()) {
      JsonDocument s;
      if (!deserializeJson(s, telemetry.sensors_json)) o["sensors"].set(s.as<JsonVariantConst>());
    }
  });
}

// ---- pairing helpers ----

bool Core::normalize_pair_code(const std::string& in, std::string& out) {
  out.clear();
  for (char c : in) {
    if (c == '-' || c == ' ') continue;
    if (c >= 'a' && c <= 'z') c = static_cast<char>(c - 'a' + 'A');
    if (c == 'I' || c == 'L') c = '1';
    if (c == 'O') c = '0';
    out.push_back(c);
  }
  if (out.size() != 8) {
    out.clear();
    return false;
  }
  for (char c : out) {
    if ((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z' && c != 'I' && c != 'L' && c != 'O' && c != 'U')) continue;
    out.clear();
    return false;
  }
  return true;
}

std::string Core::build_pair_request(const std::string& robot, const std::string& code,
                                     const std::string& agent_version, const std::string& device_kind,
                                     const std::string& driver, const std::string& capabilities_json,
                                     const std::string& name) {
  JsonDocument doc;
  if (!robot.empty()) doc["robot"] = robot;
  doc["code"] = code;
  doc["agent_version"] = agent_version;
  doc["device_kind"] = device_kind;
  JsonArray drivers = doc["drivers"].to<JsonArray>();
  if (!driver.empty()) drivers.add(driver);
  JsonDocument caps;
  if (deserializeJson(caps, capabilities_json)) caps.to<JsonObject>();
  doc["capabilities"].set(caps.as<JsonVariantConst>());
  if (!name.empty()) doc["name"] = name;
  std::string out;
  serializeJson(doc, out);
  return out;
}

bool Core::parse_paired(const std::string& frame, Paired& out) {
  JsonDocument doc;
  if (deserializeJson(doc, frame)) return false;
  out.device_id = doc["device_id"] | "";
  out.credential = doc["credential"] | "";
  out.publish_key = doc["publish_key"] | "";
  out.whip_url = doc["whip_url"] | "";
  JsonArrayConst ids = doc["robot_ids"];
  out.robot_id.clear();
  for (JsonVariantConst r : ids) {
    if (r.is<const char*>()) {
      out.robot_id = r.as<const char*>();
      break;
    }
  }
  return true;
}

} // namespace ov
