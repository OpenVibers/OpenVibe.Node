// OpenVibeNode.h — the Arduino/ESP32 transport for ov_core.
//
// This is the only part of the library that touches a board: WiFi, TLS with the ESP32 CA bundle (never setInsecure),
// the arduinoWebSockets client to the control URL, HTTPClient for POST /api/v1/pair, and Preferences (NVS) for the
// device id and credential. The protocol state machine itself is ov::Core, which the desktop harness exercises without
// any of this.
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
  // join only; the link runs from loop(). `server` is the origin, e.g. https://openvibe.bot.
  void begin(const char* ssid, const char* password, const char* robot, const char* code, const char* name,
             const char* server = "https://openvibe.bot",
             const char* capabilitiesJson = "{\"esp32\":{\"drive\":{\"type\":\"differential\"}}}");
  void loop();

  void setCallbacks(DriveFn drive, ActuatorFn actuator, StopFn stop, LogFn log);
  void setLatchCallback(ov::Core::LatchFn latch);
  void setTelemetryProvider(TelemetryFn provider);

  // Takes the owner's rotation response (POST /api/v1/devices/:id/rotate) without pairing again: it must name this
  // device. Never prints either secret.
  bool importCredential(const char* rotateResponseJson);

  bool paired() const { return !credential_.empty(); }
  ov::Core& core() { return core_; }

 private:
  void pairOverHttps();
  bool loadCredential();
  void saveCredential();
  void startLink();
  void connectWifi(const char* ssid, const char* password);
  void onWsEvent(WStype_t type, uint8_t* payload, size_t length);
  void maybePublishTelemetry();
  void wsSend(const std::string& text);
  int64_t nowMs();
  void logLine(const char* level, const std::string& message);
  static bool parseServer(const std::string& url, std::string& host, uint16_t& port, std::string& path, bool& secure);

  ov::Core core_;
  WebSocketsClient ws_;
  Preferences prefs_;
  TelemetryFn telemetry_;
  LogFn user_log_;
  uint32_t last_telemetry_ms_ = 0;
  uint32_t next_pair_ms_ = 0;
  bool link_started_ = false;
  int64_t epoch_offset_ms_ = -1;

  std::string server_;
  std::string host_;
  std::string path_ = "/device";
  uint16_t port_ = 443;
  bool secure_ = true;

  std::string device_id_;
  std::string credential_;
  std::string robot_id_;
  std::string pairing_robot_; // from the sketch, when no credential is stored yet
  std::string pairing_code_;
  std::string pairing_name_;
  std::string capabilities_json_;
  std::string firmware_;
};
