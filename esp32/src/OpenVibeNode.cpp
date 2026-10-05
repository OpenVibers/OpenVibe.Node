// OpenVibeNode.cpp — see OpenVibeNode.h.
#include "OpenVibeNode.h"

#include <cstdio>
#include <cstdlib>
#include <NetworkClient.h>
#include <ctime>
#include <utility>

namespace {

// The CA bundle the ESP32 arduino core embeds (CONFIG_MBEDTLS_CERTIFICATE_BUNDLE): the Mozilla roots, refreshed with
// the core. TLS is only ever set up from it — never setInsecure(), never a fingerprint.
extern const uint8_t rootca_crt_bundle_start[] asm("_binary_x509_crt_bundle_start");
extern const uint8_t rootca_crt_bundle_end[] asm("_binary_x509_crt_bundle_end");

size_t caBundleSize() { return static_cast<size_t>(rootca_crt_bundle_end - rootca_crt_bundle_start); }

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
    : core_([this]() { return nowMs(); }, [this](const std::string& frame) { wsSend(frame); }) {}

void OpenVibeNode::setCallbacks(DriveFn drive, ActuatorFn actuator, StopFn stop, LogFn log) {
  user_log_ = log;
  core_.set_callbacks(std::move(drive), std::move(actuator), std::move(stop),
                      [this](const char* level, const std::string& message) { logLine(level, message); });
}

void OpenVibeNode::setLatchCallback(ov::Core::LatchFn latch) { core_.set_latch_callback(std::move(latch)); }

void OpenVibeNode::setTelemetryProvider(TelemetryFn provider) { telemetry_ = std::move(provider); }

void OpenVibeNode::logLine(const char* level, const std::string& message) {
  if (user_log_) {
    user_log_(level, message);
  } else {
    Serial.printf("[openvibe][%s] %s\n", level, message.c_str());
  }
}

int64_t OpenVibeNode::nowMs() {
  const time_t t = time(nullptr);
  if (t > 1600000000 && epoch_offset_ms_ < 0) {
    epoch_offset_ms_ = static_cast<int64_t>(t) * 1000 - static_cast<int64_t>(millis());
  }
  if (epoch_offset_ms_ >= 0) return epoch_offset_ms_ + static_cast<int64_t>(millis());
  return static_cast<int64_t>(millis()); // before NTP this is a monotonic clock, not Unix; the core handles skew
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
  return !host.empty();
}

bool OpenVibeNode::loadCredential() {
  device_id_ = prefs_.getString("device_id", "").c_str();
  credential_ = prefs_.getString("credential", "").c_str();
  robot_id_ = prefs_.getString("robot_id", "").c_str();
  return !credential_.empty();
}

void OpenVibeNode::saveCredential() {
  prefs_.putString("device_id", device_id_.c_str());
  prefs_.putString("credential", credential_.c_str());
  prefs_.putString("robot_id", robot_id_.c_str());
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
  std::string code;
  if (!ov::Core::normalize_pair_code(pairing_code_, code)) {
    logLine("error", "a pairing code is 8 letters and digits, like ABCD-1234");
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
    logLine("warn", "pairing over plain http (test servers only)");
  }
  HTTPClient http;
  if (!http.begin(*client, url.c_str())) {
    logLine("error", "could not start the pairing request");
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
      logLine("error", "the pairing answer was not valid JSON");
      return;
    }
    ov::Paired paired;
    if (ov::Core::parse_paired(response.c_str(), paired)) {
      device_id_ = paired.device_id;
      credential_ = paired.credential;
      robot_id_ = paired.robot_id;
      saveCredential();
      logLine("info", "paired as " + device_id_);
      return;
    }
    logLine("error", "the pairing answer had no device id or credential");
    return;
  }
  JsonDocument problem;
  std::string code_str;
  std::string detail;
  if (!deserializeJson(problem, response.c_str())) {
    code_str = problem["code"] | "";
    detail = problem["detail"] | "";
  }
  logLine("error", "pairing failed: " + pairingMessage(status, code_str, detail));
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

  connectWifi(ssid, password);
  prefs_.begin("openvibe", false);
  if (!loadCredential()) {
    pairing_robot_ = robot != nullptr ? robot : "";
    pairing_code_ = code != nullptr ? code : "";
    pairing_name_ = name != nullptr ? name : "";
    pairOverHttps();
  }
  if (parseServer(server_, host_, port_, path_, secure_)) {
    // The link starts from loop() so begin() does not block on a socket.
  } else {
    logLine("error", "server must be https:// (or ws://localhost for tests)");
  }
}

void OpenVibeNode::startLink() {
  if (host_.empty()) return;
  // The credential goes in the upgrade's Authorization header only, never in the URL.
  char header[320];
  std::snprintf(header, sizeof(header), "Authorization: Bearer %s\r\nUser-Agent: openvibe-esp32/%s\r\n",
                credential_.c_str(), OPENVIBE_NODE_ESP32_VERSION);
  ws_.setExtraHeaders(header);
  ws_.onEvent([this](WStype_t type, uint8_t* payload, size_t length) { onWsEvent(type, payload, length); });
  ws_.setReconnectInterval(ov::kBackoffMinMS);
  if (secure_) {
    ws_.beginSslWithBundle(host_.c_str(), port_, path_.c_str(), rootca_crt_bundle_start, caBundleSize());
  } else {
    ws_.begin(host_.c_str(), port_, path_.c_str());
  }
  logLine("info", "control link starting");
}

void OpenVibeNode::onWsEvent(WStype_t type, uint8_t* payload, size_t length) {
  switch (type) {
    case WStype_CONNECTED:
      core_.connect();
      logLine("info", "control link upgraded; waiting for hello");
      break;
    case WStype_TEXT:
      core_.feed(std::string(reinterpret_cast<const char*>(payload), length));
      break;
    case WStype_DISCONNECTED: {
      // arduinoWebSockets 2.x does not surface the close code or the upgrade's HTTP status. A socket that ends before
      // hello is the protocol's 4002/401/403 case (refused credential), so the core applies the >=10 s policy; a
      // socket that ends after hello uses the documented backoff. 4003-while-connected cannot be told apart here.
      const bool was_up = core_.link_up();
      core_.on_close(was_up ? 0 : ov::kCloseInvalid);
      ws_.setReconnectInterval(core_.next_reconnect_delay_ms());
      logLine("warn", "control link down; reconnecting");
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
  const uint32_t now = millis();
  if (now - last_telemetry_ms_ < 500) return; // Bot's cap: sensor telemetry at most every 500 ms
  last_telemetry_ms_ = now;
  ov::Telemetry telemetry;
  telemetry_(&telemetry);
  core_.send_telemetry(telemetry);
}

void OpenVibeNode::loop() {
  if (!paired()) {
    const uint32_t now = millis();
    if (now >= next_pair_ms_) {
      pairOverHttps();
      next_pair_ms_ = now + 30000;
    }
    return;
  }
  if (!link_started_) {
    startLink();
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
