# zhibo

该项目提供了一个基于 [pion/mediadevices](https://github.com/pion/mediadevices) 与 [pion/webrtc](https://github.com/pion/webrtc) 的演示，包含：

* 控制服务端（`cmd/server`）：通过 HTTP 提供发送端控制及 SDP 信令转发。
* 自动启动的发送端客户端（`cmd/sender`）：默认注册到服务端，在收到 "start" 指令后采集音频、视频与屏幕共享流并推送。
* 接收端客户端（`cmd/receiver`）：可以向服务端发送开始/停止指令并处理来自发送端的 WebRTC 流。

## 目录结构

```
cmd/
  receiver/    接收端命令行程序
  sender/      发送端命令行程序
  server/      控制服务端
internal/
  server/      服务端 HTTP 逻辑实现
```

## 构建

```bash
go build ./...
```

每个组件都是独立的二进制程序，构建后可以分别运行。构建过程会自动拉取 mediadevices 与 webrtc 依赖并启用测试驱动与虚拟驱动，在没有真实硬件的环境下也能工作。

## 运行示例

1. **启动控制服务端**：

   ```bash
   go run ./cmd/server -addr :8080
   ```

2. **启动发送端**：

   ```bash
   go run ./cmd/sender -server http://localhost:8080 -id demo-sender
   ```

   发送端启动后会自动注册并轮询服务端指令。通过 `-video`、`-audio`、`-screen` 参数可以指定具体的设备 ID。

3. **启动接收端并发起传输**：

   ```bash
   go run ./cmd/receiver -server http://localhost:8080 -sender demo-sender -action start
   ```

   接收端会向服务端发送 `start` 指令并建立 WebRTC 会话。当需要停止推流时，可执行：

   ```bash
   go run ./cmd/receiver -server http://localhost:8080 -sender demo-sender -action stop
   ```

## HTTP 接口说明

服务端公开了如下控制与信令接口（均以 JSON 交互）：

* `POST /sender/register`：发送端注册。
* `GET /sender/{id}/command`：发送端长轮询获取控制指令。
* `POST /receiver/{id}/command`：接收端发送控制指令（例如 `start` / `stop`）。
* `POST /signal/{id}/offer`、`GET /signal/{id}/offer`：发送端上传 SDP Offer，接收端轮询获取。
* `POST /signal/{id}/answer`、`GET /signal/{id}/answer`：接收端上传 SDP Answer，发送端轮询获取。

## 发送端媒体采集

发送端在收到 `start` 指令后会：

1. 初始化 VP8（视频）与 Opus（音频）编码器，并将其注册到 pion/webrtc 的 MediaEngine。
2. 通过 mediadevices 获取用户音视频与屏幕捕获轨道。
3. 将轨道附加到 WebRTC PeerConnection，生成 SDP Offer 并通过服务端信令交换。
4. 收到接收端下发的 `stop` 指令后结束本次推流，等待下一次 `start`。

## 接收端行为

接收端通过 `-action start` 主动向服务端发送开始指令，并在收到 SDP Offer 后生成 Answer。连接建立后，程序会持续读取远端轨道（仅做丢弃处理，可在此基础上接入播放器或存储逻辑）。使用 `-action stop` 可中止发送端推流。

## 注意事项

* 示例中默认启用了虚拟音视频与屏幕驱动，真实部署时可根据平台选择具体驱动。
* 若需要 HTTPS 或鉴权，可在 `internal/server` 中扩展控制逻辑。
* 这是一个演示架构，实际生产环境应结合信令可靠性、错误处理与媒体呈现进行完善。
