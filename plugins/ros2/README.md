# `ros2`: ROS 2 robots

This plugin drives a ROS 2 robot from OpenVibe.Bot: TurtleBot 3 or 4, a Jackal, a home-built base, anything that takes
`geometry_msgs/msg/Twist` on a `cmd_vel` topic. The Node runs on the robot's computer, or on a computer on the same
network. The plugin turns Bot's commands into ROS 2 messages, and the robot's topics into telemetry and camera frames.
Bot's gate (roles, the owner's limits, cooldowns and the e-stop) has already judged every command before it gets here.

## What it bridges

| Bot | ROS 2 |
|---|---|
| `drive` `throttle`/`steer` | `geometry_msgs/msg/Twist` on `cmd_vel`: `linear.x = throttle × max_linear`, `angular.z = −steer × max_angular` (Bot's steer is positive to the right; ROS's z turns counter-clockwise) |
| `drive` `x`/`y`/`rotation` (`holonomic: true`) | `linear.x`, `linear.y`, `angular.z`. Bot's x is forward, y is left and rotation is counter-clockwise, as in REP 103 |
| any stop | a zero Twist: the last drive's deadline, a lost heartbeat, `stop`, `estop`, `halt`, stdin closing, before exit |
| `say` | `std_msgs/msg/String` on `say_topic`, when it is set |
| battery | `sensor_msgs/msg/BatteryState` on `battery_topic`: `percentage` (0–1) and `voltage` |
| `distance_cm` | `sensor_msgs/msg/Range` on `range_topic`, or else the nearest return within `front_arc_deg` of straight ahead on a `sensor_msgs/msg/LaserScan` (`scan_topic`) |
| `odom` | `nav_msgs/msg/Odometry` on `odom_topic`: x, y, yaw and speed |
| camera | a JPEG `sensor_msgs/msg/CompressedImage` on `image_topic`, at most `fps` frames a second; the Node publishes them as the robot's video |

## Two ways to reach ROS 2

- **`rclpy`**: the plugin is a ROS 2 node itself (named `node_name`). Run the Node where ROS 2 is installed, with its
  environment sourced (`source /opt/ros/jazzy/setup.bash`) so `rclpy` imports.
- **`rosbridge`**: JSON over WebSocket to a rosbridge server at `rosbridge_url`. Start one on the robot with
  `sudo apt install ros-$ROS_DISTRO-rosbridge-server` and `ros2 launch rosbridge_server rosbridge_websocket_launch.xml`.
  No ROS 2 libraries are needed where the Node runs, so a Raspberry Pi beside the robot, or a laptop, works. `wss://`
  works too.
- **`auto`** (the default) uses `rclpy` when it imports, otherwise `rosbridge`.

If ROS cannot be reached, the panel shows the `ros_unreachable` fault and commands are refused with it. The plugin
tries again every `retry_s` seconds; when it reconnects, it publishes a zero Twist and says `ready`, which ends the
fault.

## Setup

1. On openvibe.bot, add a robot with the **ROS 2 robot** model (`ros2.diffdrive`; `ros2.holonomic` for mecanum or
   omni wheels) and keep its pairing code.
2. On the computer that will run the Node:
   `curl -fsSL https://openvibe.bot/install | sh -s -- --robot <rob_…> --code <CODE> --driver ros2`
3. Edit `config.json` (the installer prints where it is) if your topics differ from the defaults, then restart the
   Node's service:

```json
{"plugins": [{"name": "ros2", "config": {
  "transport": "rosbridge",
  "rosbridge_url": "ws://turtlebot.local:9090",
  "cmd_vel": "/cmd_vel",
  "max_linear": 0.22,
  "max_angular": 2.0,
  "image_topic": "/camera/image_raw/compressed"
}}]}
```

TurtleBot 4 publishes its camera as `/oakd/rgb/preview/image_raw/compressed` (with `image_transport` republishing),
and its base expects `cmd_vel` under the robot's namespace when one is set.

## Configuration

| key | default | meaning |
|---|---|---|
| `transport` | `auto` | `auto`, `rclpy` or `rosbridge` |
| `rosbridge_url` | `ws://127.0.0.1:9090` | the rosbridge server (`ws://` or `wss://`) |
| `node_name` | `openvibe_node` | the ROS 2 node's name with `rclpy` |
| `cmd_vel` | `/cmd_vel` | where Twists go |
| `max_linear` | `0.3` | m/s at full throttle (0–5) |
| `max_angular` | `1.5` | rad/s at full steer (0–10) |
| `holonomic` | `false` | `x`/`y`/`rotation` drive (mecanum, omni) instead of `throttle`/`steer` |
| `battery_topic` | `/battery_state` | `sensor_msgs/msg/BatteryState`; empty turns it off |
| `range_topic` | (empty) | `sensor_msgs/msg/Range`; replaces the scan for `distance_cm` |
| `scan_topic` | `/scan` | `sensor_msgs/msg/LaserScan`; empty turns it off |
| `front_arc_deg` | `30` | how wide "straight ahead" is on the scan |
| `odom_topic` | `/odom` | `nav_msgs/msg/Odometry`; empty turns it off |
| `image_topic` | (empty) | a JPEG `sensor_msgs/msg/CompressedImage`; empty means no camera from ROS |
| `fps` | `5` | camera frames a second (0.2–15) |
| `say_topic` | (empty) | `std_msgs/msg/String` for `say`; empty refuses `say` |
| `retry_s` | `3` | seconds between reconnection attempts |

Bot's limits apply before the plugin's: the owner's `max_speed` and `max_turn` scale the command first, then
`max_linear` and `max_angular` turn it into m/s and rad/s. Set these two to what is safe for your robot indoors.
