// Basic — a differential-drive ESP32 robot on the OpenVibe Bot device protocol.
//
// Wiring (an L298N-class dual H-bridge; use your own pins if these clash with your board):
//
//   ESP32 GPIO25 -> ENA (left motor PWM)     ESP32 GPIO14 -> ENB (right motor PWM)
//   ESP32 GPIO26 -> IN1                      ESP32 GPIO32 -> IN3
//   ESP32 GPIO27 -> IN2                      ESP32 GPIO33 -> IN4
//   ESP32 GND    -> the driver's GND         Motor + battery -> the driver's +12V/GND
//
// The two enable pins are the ledc PWM channels and the four IN pins the direction. The core clamps every value to the
// owner's limits before onDrive sees it, stops the motors when a motion deadline lapses or two heartbeats are missed,
// and stops them on an e-stop or when the link closes. onStop() is the only place that zeroes the motors.
//
// Fill in the four defines below. OV_CODE is the 8-character, single-use code openvibe.bot shows for the robot
// (XXXX-XXXX, 10 minutes). After the first pairing the device id and credential are kept in NVS, so OV_CODE is only
// needed when there is no stored credential.
#include <ArduinoJson.h>
#include <OpenVibeNode.h>

#define OV_WIFI_SSID "your-ssid"
#define OV_WIFI_PASSWORD "your-password"
#define OV_ROBOT "rob_00000000000000000000000000"
#define OV_CODE "ABCD-1234"
#define OV_NAME "esp32-rover"

static const int PIN_ENA = 25;
static const int PIN_IN1 = 26;
static const int PIN_IN2 = 27;
static const int PIN_ENB = 14;
static const int PIN_IN3 = 32;
static const int PIN_IN4 = 33;
static const int PWM_FREQ = 20000;
static const int PWM_RES = 8;

OpenVibeNode node;

static void setMotor(int en, int in1, int in2, float speed) {
  speed = constrain(speed, -1.0f, 1.0f);
  digitalWrite(in1, speed >= 0.0f ? HIGH : LOW);
  digitalWrite(in2, speed >= 0.0f ? LOW : HIGH);
  ledcWrite(en, static_cast<uint32_t>(fabsf(speed) * 255.0f));
}

// The core has already clamped throttle/steer to the owner's max_speed/max_turn; mix them for a differential drive.
static void onDrive(const std::string& value, int deadline_ms) {
  (void)deadline_ms; // the core owns the motion deadline and calls onStop when it lapses
  JsonDocument doc;
  if (deserializeJson(doc, value)) return;
  const float throttle = doc["throttle"] | 0.0f;
  const float steer = doc["steer"] | 0.0f;
  const float left = constrain(throttle + steer, -1.0f, 1.0f);
  const float right = constrain(throttle - steer, -1.0f, 1.0f);
  setMotor(PIN_ENA, PIN_IN1, PIN_IN2, left);
  setMotor(PIN_ENB, PIN_IN3, PIN_IN4, right);
}

static void onActuator(const std::string& name, const std::string& value, int deadline_ms) {
  (void)name;
  (void)value;
  (void)deadline_ms; // this rover has no actuator; the core still acks the command
}

static void onStop() {
  setMotor(PIN_ENA, PIN_IN1, PIN_IN2, 0.0f);
  setMotor(PIN_ENB, PIN_IN3, PIN_IN4, 0.0f);
}

static void onLog(const char* level, const std::string& message) { Serial.printf("[%s] %s\n", level, message.c_str()); }

void setup() {
  Serial.begin(115200);
  delay(200);
  pinMode(PIN_IN1, OUTPUT);
  pinMode(PIN_IN2, OUTPUT);
  pinMode(PIN_IN3, OUTPUT);
  pinMode(PIN_IN4, OUTPUT);
  ledcAttach(PIN_ENA, PWM_FREQ, PWM_RES);
  ledcAttach(PIN_ENB, PWM_FREQ, PWM_RES);
  onStop();

  node.setCallbacks(onDrive, onActuator, onStop, onLog);
  node.setTelemetryProvider([](ov::Telemetry& telemetry) {
    telemetry.has_rssi = true;
    telemetry.rssi = WiFi.RSSI();
  });
  node.begin(OV_WIFI_SSID, OV_WIFI_PASSWORD, OV_ROBOT, OV_CODE, OV_NAME);
}

void loop() { node.loop(); }
