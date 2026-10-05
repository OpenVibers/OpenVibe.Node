// OpenVibeNode.h — the Arduino/ESP32 transport for ov_core.
//
// This is the only part of the library that touches a board: WiFi, TLS with the ESP32 CA bundle (never setInsecure),
// the arduinoWebSockets client to the control URL, HTTPClient for POST /api/v1/pair, and Preferences (NVS) for the
// device id, credential and the local kill switch. The protocol state machine itself is ov::Core, which the desktop
// harness exercises without any of this.
//
// Time comes from two clocks: esp_timer_get_time() (64-bit monotonic, so the millis() wrap at 49.7 days cannot reach
// the core's timers) drives the deadman, the motion deadline and the heartbeat cadence; Unix time is used only for the
// envelope's `ts` and the timestamps the protocol defines in Unix milliseconds.
//
// The credential and the publish key are never logged, never put in a URL and never sent anywhere but their own
// Authorization header.
#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>
#include <HTTPClient.h>
#include <Preferences.h>
#include <WebSocketsClient.h>
#include <WiFi.h>
#include <WiFiClientSecure.h>

#include <functional>
#include <string>

#include "ov_core.h"

// The library version reported in the User-Agent and status.firmware (kept in step with library.properties).
#define OPENVIBE_NODE_ESP32_VERSION "0.1.0"

class OpenVibeNode {
 public:
  using DriveFn = ov::Core::DriveFn;
  using ActuatorFn = ov::Core::ActuatorFn;
  using StopFn = ov::Core::StopFn;
  using LogFn = ov::Core::LogFn;
  using TelemetryFn = std::function<void(ov::Telemetry&)>;

  OpenVibeNode();

  // Connects WiFi, pairs with a code when no credential is stored, then opens the control link. Blocking for the WiFi
  // join only; the link runs from loop(). `server` is the origin, e.g. https://openvibe.bot. Plain http/ws is accepted
  // only for localhost (or when the library is built with OPENVIBE_ALLOW_INSECURE_HTTP for a test server).
  void begin(const char* ssid, const char* password, const char* robot, const char* code, const char* name,
             const char* server = "https://openvibe.bot",
             const char* capabilitiesJson = "{\"esp32\":{\"drive\":{\"type\":\"differential\"}}}");
  void loop();

  void setCallbacks(DriveFn drive, ActuatorFn actuator, StopFn stop, LogFn log);
  void setLatchCallback(ov::Core::LatchFn latch);
  void setTelemetryProvider(TelemetryFn provider);

  // The local kill switch (protocol §3): latched, persisted in NVS, restored in begin() before WiFi comes up. Only
  // setLocalStop(false)/resume() clears it; the server's `estop` latched:false never does.
  void setLocalStop(bool on);
  void resume() { setLocalStop(false); }

  // Takes the owner's rotation response (POST /api/v1/devices/:id/rotate) without pairing again: it must name this
  // device. Never prints either secret. Uses the new credential on the next reconnect.
  bool importCredential(const char* rotateResponseJson);

  bool paired() const { return !credential_.empty(); }
  ov::Core& core() { return core_; }

 private:
  void pairOverHttps(); // retries transport/5xx errors with backoff; a 4xx answer is definitive
  void schedulePairRetry(int64_t now);
  bool loadCredential();
  bool saveCredential();
  bool startLink();
  void connectWifi(const char* ssid, const char* password);
  void onWsEvent(WStype_t type, uint8_t* payload, size_t length);
  void onLatchChanged(bool remote, bool local);
  void maybePublishTelemetry();
  void wsSend(const std::string& text);
  int64_t monotonicMs() const;
  int64_t unixMs();
  bool buildExtraHeaders();
  void logLine(const char* level, const std::string& message);
  static bool parseServer(const std::string& url, std::string& host, uint16_t& port, std::string& path, bool& secure);

  ov::Core core_;
  WebSocketsClient ws_;
  Preferences prefs_;
  TelemetryFn telemetry_;
  ov::Core::LatchFn user_latch_;
  LogFn user_log_;
  int64_t last_telemetry_ms_ = 0;
  int64_t next_pair_ms_ = 0;
  int pair_attempts_ = 0;
  bool pairing_given_up_ = false;
  bool link_started_ = false;
  bool prefs_open_ = false;
  int64_t epoch_offset_ms_ = -1;

  std::string server_;
  std::string host_;
  std::string path_ = "/device";
  uint16_t port_ = 443;
  bool secure_ = true;
  std::string extra_headers_; // Authorization/User-Agent, rebuilt on rotation and kept for the link's lifetime

  std::string device_id_;
  std::string credential_;
  std::string robot_id_;
  std::string pairing_robot_; // from the sketch, when no credential is stored yet
  std::string pairing_code_;
  std::string pairing_name_;
  std::string capabilities_json_;
  std::string firmware_;
};
