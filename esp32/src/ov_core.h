// ov_core.h — the OpenVibe Bot device protocol, in portable C++17.
//
// This is the whole stateful device side of docs/protocol.md sections 1–3 (minus jobs and video): the frame envelope,
// hello/config/command/estop/heartbeat_ack handling, clamping before any driver sees a value, ack/nack, the heartbeat
// cadence and deadman, the motion deadline, the e-stop latch and the close-code policy. It has no Arduino headers:
// it includes only ArduinoJson (v7) and the standard library, so the same file runs on an ESP32 and in the desktop
// conformance harness (esp32/test).
//
// Time is injected as a `now_ms` function returning Unix milliseconds, and every side effect is a callback
// (send/heartbeat frames, drive, actuator, stop, log, latch change). The Arduino transport lives in OpenVibeNode.h;
// nothing here touches WiFi, TLS, NVS or a board.
//
// Source of truth: docs/protocol.md and internal/protocol in OpenVibe.Node. Where this file diverges from the Go Node
// it is because the ESP32 has no plugin supervisor, no worker and no WHIP publisher, and says so in the protocol's own
// language (unknown kinds are `unsupported`, a job is `class not available`).
#pragma once

#include <ArduinoJson.h>

#include <cstdint>
#include <functional>
#include <map>
#include <string>
#include <utility>
#include <vector>

namespace ov {

// Envelope version (protocol §Frames).
inline constexpr int kVersion = 1;

// Timing defaults (protocol §Commands, §Heartbeat, deadman, reconnect).
inline constexpr int kDefaultDeadlineMS = 300;
inline constexpr int kDefaultMaxCommandMS = 1000;
inline constexpr int kDefaultHeartbeatMS = 1000;
inline constexpr int64_t kHelloAllowanceMS = 10000;
inline constexpr int kCredentialRetryMinMS = 10000; // 4002/4003/401/403: never sooner
inline constexpr int kBackoffMinMS = 500;
inline constexpr int kBackoffMaxMS = 30000;

// Close codes the server ends the socket with (protocol §2).
inline constexpr int kCloseReplaced = 4000;
inline constexpr int kCloseInvalid = 4002;
inline constexpr int kCloseRevoked = 4003;

// Fault codes carried by nack.fault_code (protocol §Fault codes).
inline constexpr const char* kFaultBadFrame = "bad_frame";
inline constexpr const char* kFaultBadValue = "bad_value";
inline constexpr const char* kFaultUnsupported = "unsupported";
inline constexpr const char* kFaultNotAllowed = "not_allowed";
inline constexpr const char* kFaultEstopped = "estopped";
inline constexpr const char* kFaultLocalStop = "local_stop";
inline constexpr const char* kFaultNotReady = "not_ready";
inline constexpr const char* kFaultExpired = "expired";
inline constexpr const char* kFaultShuttingDown = "shutting_down";

// Job refusal messages (protocol §Jobs), for a device with no worker.
inline constexpr const char* kJobBadID = "missing or malformed job id";
inline constexpr const char* kJobBadFrame = "malformed job";
inline constexpr const char* kJobUnknownClass = "unknown class";
inline constexpr const char* kJobNoArtifact = "function or code job needs an artifact with an exact version";
inline constexpr const char* kJobNetUnsupported = "net policy not supported";
inline constexpr const char* kJobBadInputs = "invalid inputs";
inline constexpr const char* kJobInputsRefused = "job inputs not supported";
inline constexpr const char* kJobBadLimits = "missing or invalid args, ttl_ms or limits";
inline constexpr const char* kJobNotAvailable = "class not available";

// Command kinds understood by the protocol (protocol §Commands). The ESP32 driver implements drive, actuator and
// halt; ptz, say and display are valid kinds this device has no driver for, so they are `nack unsupported`.
inline constexpr const char* kKindDrive = "drive";
inline constexpr const char* kKindActuator = "actuator";
inline constexpr const char* kKindPTZ = "ptz";
inline constexpr const char* kKindSay = "say";
inline constexpr const char* kKindDisplay = "display";
inline constexpr const char* kKindHalt = "halt";

// The effective owner limits: the stricter of the server's config and the device's local caps (protocol step 6).
struct Limits {
  double max_speed = 1.0; // throttle, x, y
  double max_turn = 1.0;  // steer, rotation
  int max_command_ms = kDefaultMaxCommandMS;
};

// Telemetry the transport feeds in; every field is optional except the object itself (protocol §Device → server).
struct Telemetry {
  bool has_battery = false;
  double battery = 0.0;
  bool has_voltage = false;
  double voltage = 0.0;
  bool has_rssi = false;
  int rssi = 0;
  std::string sensors_json; // a JSON object, or empty
};

// The pairing answer (`paired` frame / POST /api/v1/pair body), parsed for the transport.
struct Paired {
  std::string device_id;
  std::string credential;
  std::string publish_key;
  std::string whip_url;
  std::string robot_id;
};

// One remembered command result: a repeated `id` is answered with it and never executed again (step 1).
struct DedupResult {
  bool ok = false;
  std::string fault;
  std::string message;
};

class Core {
 public:
  using NowFn = std::function<int64_t()>;                 // Unix milliseconds
  using SendFn = std::function<void(const std::string&)>; // one text frame, already encoded
  using DriveFn = std::function<void(const std::string& value, int deadline_ms)>;
  using ActuatorFn = std::function<void(const std::string& name, const std::string& value, int deadline_ms)>;
  using StopFn = std::function<void()>;
  using LogFn = std::function<void(const char* level, const std::string& message)>;
  using LatchFn = std::function<void(bool remote, bool local)>;

  Core(NowFn now, SendFn send);

  // --- configuration, set once by the transport/sketch ---
  void set_callbacks(DriveFn drive, ActuatorFn actuator, StopFn stop, LogFn log);
  void set_latch_callback(LatchFn latch);
  // The local caps, applied on top of the server's config (the stricter wins).
  void set_local_limits(double max_speed, double max_turn, int max_command_ms);
  // The device descriptor the protocol carries: status.firmware, the driver name and the per-driver capabilities
  // object. Nothing here ever adds a `worker` capability (this device runs no jobs).
  void set_descriptor(const std::string& firmware, const std::string& driver, const std::string& capabilities_json);

  const std::string& firmware() const { return firmware_; }
  const std::string& driver() const { return driver_; }
  const std::string& capabilities_json() const { return capabilities_json_; }

  // --- connection lifecycle, called by the transport ---
  void connect();                     // the WebSocket upgrade succeeded; wait for hello
  void feed(const std::string& text); // one server text frame
  void tick();                        // call often: heartbeat cadence, deadman, motion deadline
  void on_close(int code);            // the socket ended; 0 when the transport cannot know the code

  // After on_close: jittered delay before the next connect (>= 10 s for 4002/4003/401/403).
  int next_reconnect_delay_ms() const { return next_delay_ms_; }
  bool credential_refused() const { return credential_refused_; }
  bool link_lost() const { return link_lost_; } // the core decided to drop the link (hello/deadman timeout)
  void clear_link_lost() { link_lost_ = false; }

  // --- state the transport/driver may read ---
  bool link_up() const { return up_; }
  bool configured() const { return configured_; }
  bool estopped() const { return remote_stop_ || local_stop_; }
  bool remote_estopped() const { return remote_stop_; }
  bool local_stop() const { return local_stop_; }
  const std::string& device_id() const { return device_id_; }
  const std::string& session_id() const { return session_id_; }
  const std::vector<std::string>& robot_ids() const { return robot_ids_; }

  void set_local_stop(bool on); // the local kill switch; latched, persisted by the transport

  // --- frames the transport may need to send ---
  void send_status();
  void send_estop_state();
  void send_telemetry(const Telemetry& telemetry);

  // --- pairing helpers (protocol §1), portable so the harness can check them ---
  // Normalises a code as Bot does: upper case, dash and spaces removed, I/L → 1 and O → 0.
  static bool normalize_pair_code(const std::string& in, std::string& out);
  // Builds the POST /api/v1/pair body (the `pair` frame without the envelope).
  static std::string build_pair_request(const std::string& robot, const std::string& code,
                                        const std::string& agent_version, const std::string& device_kind,
                                        const std::string& driver, const std::string& capabilities_json,
                                        const std::string& name);
  // Parses Bot's `paired` frame into the same fields POST /api/v1/pair answers with.
  static bool parse_paired(const std::string& frame, Paired& out);

 private:
  void emit(const char* type, const std::function<void(JsonObject)>& fill);
  void log(const char* level, const std::string& message) const;
  void notify_latch() const;

  void handle_hello(JsonDocument& doc, int64_t now);
  void handle_config(JsonDocument& doc);
  void handle_command(JsonDocument& doc, int64_t ts, int64_t now);
  void handle_estop(JsonDocument& doc);
  void handle_heartbeat_ack(JsonDocument& doc, int64_t now);
  void handle_job(JsonDocument& doc);

  void observe(int64_t ts, int64_t now);
  int64_t server_now(int64_t ts, int64_t now) const;
  bool clamp_command(const std::string& kind, JsonVariantConst value, std::string& out, std::string& fault,
                     std::string& message) const;
  void send_ack(const std::string& id, int64_t latency_ms);
  void send_nack(const std::string& id, const char* fault, const std::string& message);
  void remember(const std::string& id, const DedupResult& result);
  void remember_job(const std::string& id, const char* fault, const std::string& message);
  bool recall(const std::string& id, DedupResult& result) const;
  int normal_backoff_ms();
  uint32_t next_random();
  void stop_motion();
  void send_heartbeat();
  void set_remote_stop(bool on, const std::string& reason, bool notify);

  NowFn now_;
  SendFn send_;
  DriveFn drive_;
  ActuatorFn actuator_;
  StopFn stop_;
  LogFn log_fn_;
  LatchFn latch_fn_;

  std::string firmware_ = "openvibe-esp32-0.0.0";
  std::string driver_ = "esp32";
  std::string capabilities_json_ = "{}";

  Limits local_limits_{};
  Limits limits_{};

  bool up_ = false;
  bool configured_ = false;
  bool remote_stop_ = false;
  bool local_stop_ = false;
  bool credential_refused_ = false;
  bool link_lost_ = false;
  std::string remote_reason_;
  std::string device_id_;
  std::string session_id_;
  std::vector<std::string> robot_ids_;

  bool allowed_present_ = false;
  std::map<std::string, bool> allowed_;

  uint64_t seq_ = 0;
  int64_t last_rx_ms_ = 0;
  int64_t next_hb_ms_ = 0;
  int hb_ms_ = kDefaultHeartbeatMS;
  int deadman_ms_ = 2 * kDefaultHeartbeatMS;

  int64_t motion_deadline_ms_ = 0;

  bool has_skew_ = false;
  int64_t skew_ms_ = 0;

  int64_t last_rtt_ms_ = 0;
  bool has_rtt_ = false;

  struct HbSend {
    int64_t t;
    int64_t at;
  };
  std::map<uint64_t, HbSend> hb_sent_;

  std::map<std::string, DedupResult> dedup_;
  std::vector<std::string> dedup_order_;
  std::map<std::string, std::pair<std::string, std::string>> job_answers_;
  std::vector<std::string> job_order_;

  int reconnect_attempt_ = 0;
  int next_delay_ms_ = kBackoffMinMS;
  uint32_t rng_state_ = 0x9e3779b9u;
};

} // namespace ov
