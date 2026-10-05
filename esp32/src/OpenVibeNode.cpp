// OpenVibeNode.cpp — see OpenVibeNode.h.
#include "OpenVibeNode.h"

#include <cstdio>
#include <cstdlib>
#include <NetworkClient.h>
#include <ctime>
#include <esp_timer.h>
#include <utility>

namespace {

// The CA bundle the ESP32 arduino core embeds (CONFIG_MBEDTLS_CERTIFICATE_BUNDLE, on by default): the Mozilla roots,
// refreshed with the core. TLS is only ever set up from it — never setInsecure(), never a fingerprint.
extern const uint8_t rootca_crt_bundle_start[] asm("_binary_x509_crt_bundle_start");
extern const uint8_t rootca_crt_bundle_end[] asm("_binary_x509_crt_bundle_end");

size_t caBundleSize() { return static_cast<size_t>(rootca_crt_bundle_end - rootca_crt_bundle_start); }

// The Authorization/User-Agent block is built as a std::string with no truncation; a credential beyond this is refused
// rather than letting a fixed buffer cut the header short (which would leave it unterminated).
constexpr size_t kMaxHeaderBytes = 512;

// The sentence the person at the board should read for a Bot pairing refusal (mirrors internal/link/pair.go).
std::string pairingMessage(int status, const std::string& code, const std::string& detail) {
  if (code == "bot.invalid_pairing_code") return "a pairing code is 8 letters and digits, like ABCD-1234";
  if (code == "bot.pairing_code_invalid")
    return "that is not the pairing code for this robot; check it, or ask for a new one on openvibe.bot";
  if (code == "bot.pairing_code_locked")
    return "too many wrong tries: the code is dead; ask for a new one on openvibe.bot";
  if (code == "bot.pairing_code_used") return "the pairing code was already used; ask for a new one on openvibe.bot";
  if (code == "bot.pairing_code_expired")
    return "the pairing code expired (codes last 10 minutes); ask for a new one on openvibe.bot";
  if (code == "bot.no_pairing_code") return "this robot has no live pairing code; ask for a new one on openvibe.bot";
  if (status == 429) return "too many pairing attempts from here; wait a minute and try again";
  if (!detail.empty()) return detail;
  return "pairing refused with HTTP " + std::to_string(status);
}

} // namespace

OpenVibeNode::OpenVibeNode()
    : core_([this]() { return monotonicMs(); }, [this]() { return unixMs(); },
            [this](const std::string& frame) { wsSend(frame); }) {
  // Persist the kill switch through the core's latch callback (setLatchCallback() only replaces the user's half).
  core_.set_latch_callback([this](bool remote, bool local) { onLatchChanged(remote, local); });
}

void OpenVibeNode::setCallbacks(DriveFn drive, ActuatorFn actuator, StopFn stop, LogFn log) {
  user_log_ = log;
  core_.set_callbacks(std::move(drive), std::move(actuator), std::move(stop),
                      [this](const char* level, const std::string& message) { logLine(level, message); });
}

void OpenVibeNode::setLatchCallback(ov::Core::LatchFn latch) { user_latch_ = std::move(latch); }

void OpenVibeNode::setTelemetryProvider(TelemetryFn provider) { telemetry_ = std::move(provider); }

void OpenVibeNode::setLocalStop(bool on) { core_.set_local_stop(on); }

void OpenVibeNode::logLine(const char* level, const std::string& message) {
  if (user_log_) {
    user_log_(level, message);
  } else {
    Serial.printf("[openvibe][%s] %s\n", level, message.c_str());
  }
}

int64_t OpenVibeNode::monotonicMs() const { return static_cast<int64_t>(esp_timer_get_time() / 1000); }

int64_t OpenVibeNode::unixMs() {
  // Unix milliseconds for the envelope's `ts` and the protocol's Unix timestamps only; every timer in the core uses the
  // monotonic clock, so an NTP step (or its absence) cannot move the deadman, a deadline or the heartbeat cadence.
  const time_t t = time(nullptr);
  if (t > 1600000000 && epoch_offset_ms_ < 0) {
    epoch_offset_ms_ = static_cast<int64_t>(t) * 1000 - monotonicMs();
  }
  if (epoch_offset_ms_ >= 0) return epoch_offset_ms_ + monotonicMs();
  return monotonicMs(); // before NTP there is no Unix time; report the monotonic clock instead of a bogus epoch
}

void OpenVibeNode::wsSend(const std::string& text) { ws_.sendTXT(text.c_str(), text.size()); }

bool OpenVibeNode::parseServer(const std::string& url, std::string& host, uint16_t& port, std::string& path,
                               bool& secure) {
  std::string rest;
  if (url.rfind("https://", 0) == 0) {
    secure = true;
    port = 443;
    rest = url.substr(8);
  } else if (url.rfind("http://", 0) == 0) {
    secure = false;
    port = 80;
    rest = url.substr(7);
  } else if (url.rfind("wss://", 0) == 0) {
    secure = true;
    port = 443;
    rest = url.substr(6);
  } else if (url.rfind("ws://", 0) == 0) {
    secure = false;
    port = 80;
    rest = url.substr(5);
  } else {
    return false;
  }
  const size_t slash = rest.find('/');
  std::string authority = slash == std::string::npos ? rest : rest.substr(0, slash);
  const std::string tail = slash == std::string::npos ? "" : rest.substr(slash);
  path = tail.empty() || tail == "/" ? "/device" : tail + "/device";
  const size_t colon = authority.find(':');
  if (colon != std::string::npos) {
    host = authority.substr(0, colon);
    port = static_cast<uint16_t>(std::atoi(authority.substr(colon + 1).c_str()));
  } else {
    host = authority;
  }
  if (host.empty()) return false;
#ifndef OPENVIBE_ALLOW_INSECURE_HTTP
  // Protocol §1: plain http is only for localhost test servers. Anywhere else it would send the pairing code and then
  // the Bearer credential in cleartext, so the server is refused and no link starts. Build the library with
  // -DOPENVIBE_ALLOW_INSECURE_HTTP to opt in (and only against a test server).
  if (!secure && host != "localhost" && host != "127.0.0.1") return false;
#endif
  return true;
}

bool OpenVibeNode::loadCredential() {
  device_id_ = prefs_.getString("device_id", "").c_str();
  credential_ = prefs_.getString("credential", "").c_str();
  robot_id_ = prefs_.getString("robot_id", "").c_str();
  return !credential_.empty();
}

bool OpenVibeNode::saveCredential() {
  // putString returns the bytes written, 0 on failure. A silently failed write would burn the single-use code and leave
  // the board unpaired after the next reboot, so say so loudly.
  bool ok = prefs_open_ && prefs_.putString("device_id", device_id_.c_str()) > 0;
  ok = prefs_open_ && prefs_.putString("credential", credential_.c_str()) > 0 && ok;
  if (!robot_id_.empty()) ok = prefs_open_ && prefs_.putString("robot_id", robot_id_.c_str()) > 0 && ok;
  if (!ok) logLine("error", "could not write the credential to NVS; it will be lost on the next reboot");
  return ok;
}

void OpenVibeNode::onLatchChanged(bool remote, bool local) {
  // Protocol §3: the local kill switch is latched and persisted; only resume() clears it. The remote latch is re-applied
  // from each connection's config.estop_latched (including after a reboot), so it is deliberately not stored.
  if (prefs_open_ && local != prefs_.getBool("local_stop", false)) {
    if (prefs_.putBool("local_stop", local) == 0) {
      logLine("error", "could not persist the local kill switch in NVS");
    }
  }
  if (user_latch_) user_latch_(remote, local);
}

void OpenVibeNode::connectWifi(const char* ssid, const char* password) {
  WiFi.mode(WIFI_STA);
  WiFi.begin(ssid, password);
  logLine("info", "connecting to WiFi");
  const uint32_t start = millis();
  while (WiFi.status() != WL_CONNECTED && millis() - start < 20000) {
    delay(200);
  }
  if (WiFi.status() == WL_CONNECTED) {
    logLine("info", "WiFi connected");
  } else {
    logLine("error", "WiFi did not connect; will keep trying in loop()");
  }
}

void OpenVibeNode::pairOverHttps() {
  const int64_t now = monotonicMs();
  std::string code;
  if (!ov::Core::normalize_pair_code(pairing_code_, code)) {
    logLine("error", "a pairing code is 8 letters and digits, like ABCD-1234");
    pairing_given_up_ = true; // the sketch's code is malformed; retrying the same bytes cannot help
    return;
  }
  const std::string body = ov::Core::build_pair_request(pairing_robot_, code, OPENVIBE_NODE_ESP32_VERSION, "onboard",
                                                        "esp32", capabilities_json_, pairing_name_);
  const std::string url = server_ + "/api/v1/pair";
  WiFiClientSecure secure_client;
  WiFiClient plain_client; // WiFiClient is NetworkClient in core 3.x
  NetworkClient* client = &plain_client;
  if (secure_) {
    secure_client.setCACertBundle(rootca_crt_bundle_start, caBundleSize());
    client = &secure_client;
  } else {
    logLine("warn", "pairing over plain http (test server)");
  }
  HTTPClient http;
  if (!http.begin(*client, url.c_str())) {
    logLine("warn", "could not start the pairing request; retrying");
    schedulePairRetry(now);
    return;
  }
  http.setTimeout(15000);
  http.addHeader("Content-Type", "application/json");
  const int status = http.POST(String(body.c_str()));
  const String response = http.getString();
  http.end();
  if (status >= 200 && status < 300) {
    JsonDocument doc;
    if (deserializeJson(doc, response.c_str())) {
      logLine("error", "the pairing answer was not valid JSON; the code is spent, pair again with a new one");
      pairing_given_up_ = true;
      return;
    }
    ov::Paired paired;
    if (!ov::Core::parse_paired(response.c_str(), paired) || paired.device_id.empty() || paired.credential.empty()) {
      logLine("error", "the pairing answer had no device id or credential; the code is spent, pair again with a new one");
      pairing_given_up_ = true;
      return;
    }
    device_id_ = paired.device_id;
    credential_ = paired.credential;
    robot_id_ = paired.robot_id;
    if (saveCredential()) {
      logLine("info", "paired as " + device_id_);
    } else {
      logLine("error", "paired as " + device_id_ + ", but the credential could not be saved; it is only in RAM");
    }
    return;
  }
  if (status >= 400 && status < 500) {
    // A definitive refusal (4xx, including a locked or used code): retrying the same code only adds wrong tries and
    // makes Bot's lock worse, so stop until the board is rebooted with a fresh code.
    JsonDocument problem;
    std::string code_str;
    std::string detail;
    if (!deserializeJson(problem, response.c_str())) {
      code_str = problem["code"] | "";
      detail = problem["detail"] | "";
    }
    logLine("error", "pairing refused: " + pairingMessage(status, code_str, detail) + "; not retrying this code");
    pairing_given_up_ = true;
    return;
  }
  if (status > 0) {
    logLine("warn", "pairing failed with HTTP " + std::to_string(status) + "; retrying with backoff");
  } else {
    logLine("warn", "the pairing request failed (transport error); retrying with backoff");
  }
  schedulePairRetry(now);
}

void OpenVibeNode::schedulePairRetry(int64_t now) {
  ++pair_attempts_;
  int64_t backoff = 30000;
  for (int i = 1; i < pair_attempts_ && backoff < 300000; ++i) backoff *= 2;
  next_pair_ms_ = now + backoff;
}

bool OpenVibeNode::importCredential(const char* rotateResponseJson) {
  if (credential_.empty()) {
    logLine("error", "pair first: there is no stored credential to rotate");
    return false;
  }
  JsonDocument doc;
  if (deserializeJson(doc, rotateResponseJson)) {
    logLine("error", "the rotation response was not valid JSON");
    return false;
  }
  const std::string new_id = doc["device"]["id"] | "";
  const std::string new_credential = doc["credential"] | "";
  if (new_id.empty() || new_credential.empty()) {
    logLine("error", "the rotation response had no device id or credential");
    return false;
  }
  if (new_id != device_id_) {
    logLine("error", "the rotation response is for another device; refusing to import it");
    return false;
  }
  credential_ = new_credential;
  saveCredential();
  // Rebuild the header so the next reconnect presents the new credential (the current socket keeps the old one until
  // it closes, like the Go Node, which only picks a rotated credential up on restart).
  if (link_started_ && buildExtraHeaders()) {
    ws_.setExtraHeaders(extra_headers_.c_str());
  }
  logLine("info", "imported the owner's rotated credential");
  return true;
}

void OpenVibeNode::begin(const char* ssid, const char* password, const char* robot, const char* code, const char* name,
                         const char* server, const char* capabilitiesJson) {
  server_ = server;
  capabilities_json_ = capabilitiesJson;
  firmware_ = std::string("openvibe-esp32-") + OPENVIBE_NODE_ESP32_VERSION;
  core_.set_descriptor(firmware_, "esp32", capabilities_json_);
  core_.set_local_limits(1.0, 1.0, ov::kDefaultMaxCommandMS);

  // Parse the server before pairing: the scheme decides TLS for both the pair POST and the link, and a cleartext
  // server that is not localhost is refused outright.
  if (!parseServer(server_, host_, port_, path_, secure_)) {
    logLine("error",
            "server must be https:// or wss:// (plain http:// or ws:// is only for localhost; build with "
            "OPENVIBE_ALLOW_INSECURE_HTTP for a test server)");
  }

  // NVS before WiFi, so a board that rebooted with the local kill switch latched comes back latched.
  prefs_open_ = prefs_.begin("openvibe", false);
  if (!prefs_open_) logLine("error", "could not open NVS; pairing and the kill switch will not persist");
  if (prefs_open_ && prefs_.getBool("local_stop", false)) {
    core_.set_local_stop(true);
    logLine("warn", "the local kill switch was latched before the reboot; call resume() to clear it");
  }

  connectWifi(ssid, password);
  if (!loadCredential()) {
    pairing_robot_ = robot != nullptr ? robot : "";
    pairing_code_ = code != nullptr ? code : "";
    pairing_name_ = name != nullptr ? name : "";
    // loop() redeems the code: transport and 5xx errors retry with backoff, a 4xx refusal stops until a reboot.
  }
}

bool OpenVibeNode::buildExtraHeaders() {
  const std::string header = std::string("Authorization: Bearer ") + credential_ + "\r\nUser-Agent: openvibe-esp32/" +
                             OPENVIBE_NODE_ESP32_VERSION + "\r\n";
  if (header.size() > kMaxHeaderBytes) {
    logLine("error", "the stored credential is too long for the Authorization header; refusing to connect");
    return false;
  }
  extra_headers_ = header;
  return true;
}

bool OpenVibeNode::startLink() {
  if (host_.empty()) return false; // parseServer() refused it; begin() already logged why
  if (!buildExtraHeaders()) return false;
  // The credential goes in the upgrade's Authorization header only, never in the URL. extra_headers_ is a member, and
  // the library copies it into its own String, so neither side can outlive the other.
  ws_.setExtraHeaders(extra_headers_.c_str());
  ws_.onEvent([this](WStype_t type, uint8_t* payload, size_t length) { onWsEvent(type, payload, length); });
  ws_.setReconnectInterval(ov::kBackoffMinMS);
  if (secure_) {
    ws_.beginSslWithBundle(host_.c_str(), port_, path_.c_str(), rootca_crt_bundle_start, caBundleSize());
  } else {
    logLine("warn", "control link over plain ws:// (test server)");
    ws_.begin(host_.c_str(), port_, path_.c_str());
  }
  logLine("info", "control link starting");
  return true;
}

void OpenVibeNode::onWsEvent(WStype_t type, uint8_t* payload, size_t length) {
  switch (type) {
    case WStype_CONNECTED:
      core_.connect();
      logLine("info", "control link upgraded; waiting for hello");
      break;
    case WStype_TEXT:
      // The core bounds the frame and refuses an oversize one without this side copying it first.
      core_.feed(reinterpret_cast<const char*>(payload), length);
      break;
    case WStype_DISCONNECTED: {
      // arduinoWebSockets 2.x does not surface the close code or the upgrade's HTTP status. The core treats a drop
      // before hello as the >=10 s refused-credential case and a drop after hello as the normal backoff; its log says
      // the pre-hello cause is uncertain. A 4003 sent while connected cannot be told apart here.
      const bool was_up = core_.link_up();
      core_.on_close(0);
      ws_.setReconnectInterval(core_.next_reconnect_delay_ms());
      if (was_up) {
        logLine("warn", "control link down; reconnecting");
      } else {
        logLine("warn",
                "control link down before hello (the transport cannot see the close code: refused credential, "
                "network drop or timeout); reconnecting");
      }
      break;
    }
    case WStype_ERROR:
      logLine("warn", "control link error");
      break;
    default:
      break;
  }
}

void OpenVibeNode::maybePublishTelemetry() {
  if (!telemetry_ || !core_.link_up()) return;
  const int64_t now = monotonicMs();
  if (now - last_telemetry_ms_ < 500) return; // Bot's cap: sensor telemetry at most every 500 ms
  last_telemetry_ms_ = now;
  ov::Telemetry telemetry;
  telemetry_(telemetry);
  core_.send_telemetry(telemetry);
}

void OpenVibeNode::loop() {
  if (!paired()) {
    // Pairing is retried with backoff; after a definitive 4xx refusal pairOverHttps() stops trying until a reboot.
    if (pairing_given_up_) {
      delay(1000); // permanently unpaired: do not spin the loop
      return;
    }
    if (monotonicMs() >= next_pair_ms_) pairOverHttps();
    return;
  }
  if (!link_started_) {
    if (!startLink()) {
      delay(1000); // a misconfigured server or credential: do not spin, retry shortly
      return;
    }
    link_started_ = true;
  }
  // tick() always runs so the deadman still stops the motors if WiFi drops and no frame arrives.
  ws_.loop();
  core_.tick();
  if (core_.link_lost()) {
    core_.clear_link_lost();
    ws_.disconnect(); // the DISCONNECTED event applies the backoff and stops the actuators
  }
  maybePublishTelemetry();
}
