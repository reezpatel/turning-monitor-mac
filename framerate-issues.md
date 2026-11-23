# 帧率问题：测量结果与已验证结论

> 状态：**部分解决**。本文档记录 2026-11-22/23 在两块图灵 12.3"（720×1920）
> 上测得的全部数字、已验证的修复、以及被否决的方案。后续优化从这里继续，
> 不要重试标了 ❌ 的路线 —— 每一条都在真机上验证过失败方式。

## 测量基线（单面板，1920×720 landscape，30fps/16Mbit/s）

| 场景 | 帧率 | 数据率 | 队列深度 |
|---|---|---|---|
| 纯 USB 推流（`turzx video` 预编码文件，绕过采集） | **~27 fps** | ~460 KB/s | — |
| 纯采集编码（虚拟屏 + ScreenCaptureKit + VideoToolbox，无 USB） | **~25 fps** | ~10 KB/s | — |
| monitor 完整管线（采集→编码→USB，串行循环，原始代码） | **15–17 fps** | ~100–160 KB/s | 0 |
| monitor 完整管线（v2 修复后：1ms probe + 逐帧队列节奏） | **~15.5 fps** | ~145 KB/s | 0，55 秒 1400 帧零重置 |

关键结论：
- 面板解码/USB 链路本身能跑 27fps，**面板不是瓶颈**
- 采集端能产出 25fps，**采集不是瓶颈**
- `queue=0` 恒定：面板始终在等主机喂 —— **主机侧的每帧成本是瓶颈**
- 27 与 15.5 之间的差距 ≈ 每帧 ~25ms 的额外串行成本

## 每帧成本的构成（单 chunk 帧的串行路径）

```
session.Next()     等下一帧到达        （与 USB 串行时纯属闲置）
d.Send(chunk):     bulk OUT 写          ~几 ms（数据本身）
                  读设备应答            ~几 ms（往返）
                  flushLocked() 探测    3ms × 5 = 最多 15ms（v2 后 1ms × 5）
waitForQueue():    CmdStreamStatus 往返 ~几 ms（v2 起每帧一次）
```

## ✅ 已验证的修复（v2，已合入）

1. **flushTimeout 3ms → 1ms**。probe 探测在端点空时按超时全额支付，
   3ms×5 每帧最多 15ms。1ms 后空端点成本降到 ~1ms。
   （`internal/usb/transport.go`）
2. **逐帧队列节奏**。`SendVideoFrame` 现在在每个 chunk 后轮询
   `CmdStreamStatus`（原来只在同一帧的 chunk 之间）。这是流不溢出设备
   播放队列的保证。（`internal/usb/ops.go`）

   v2 结果：55 秒连续推流、1400+ 帧、**零会话重置**、15.5fps 稳定。

## ❌ 已否决的方案（勿重试）

### 1. Fire-and-forget chunk（不等应答直接写下一个）
- **现象**：fps 崩到 0.5，帧尺寸异常（70KB），随后
  `usb write: transfer was cancelled`，会话崩溃循环。
- **根因**：协议是严格请求-响应。设备的 IN 端点应答缓冲有限；应答不被
  读取时端点回压，固件最终**完全停止接受写入**。ack 计数器 + 后台
  drain goroutine（100ms ticker）也救不回来 —— 应答节奏本身就是节流。
- **附带损伤**：崩溃循环的反复重开会把面板打到串口号丢失
  （`turzx info` 显示 serial 为空），需要物理重插恢复。
  本会话中发生过多次，均为此原因。

### 2. 跳过流式路径的 flushLocked（只在非流式命令后 drain）
- **现象**：连接后第一个 prelude 写就挂满 5s 超时，
  `stop-video: usb write: transfer was cancelled`。
- **根因**：**每次应答后的 drain 是 load-bearing，不是卫生措施**。
  设备在某些命令后会发出多于调用者读取数量的块；不排干则端点逐渐
  回压直到固件拒写。表现为面板"卡死"，只能断电恢复。
- **教训**：原版 3ms×5 的 flush 又慢又必要 —— 慢的是实现（固定超时
  轮询），必要的是语义（读到空）。v2 保留了语义，只把成本降到 1ms。

### 3. 流水线化帧循环（采集与 USB 发送重叠）
- **做法**：`session.Next()` 与 `SendVideoFrame` 各占一个 goroutine，
  发上一帧的同时采集下一帧。
- **现象**：fps 从 10 衰减到 ~1（约 100 帧内），无会话重置。
- **根因（已定位，未修复）**：`session.Next()` 返回的切片**只保证到
  下次调用前有效**（内部复用单一缓冲）。发送 goroutine 还在传时，
  下一次 `Next()` 已覆写字节 —— H.264 流被破坏，面板解码器失步，
  表现为越跑越慢。修复方向已写入代码：发送前 copy 一份
  （`cmd/turzx/main.go` streamToPanel，缓冲复用 + `copy`）。
  **该修复已写但未在真机验证** —— 下次继续从这里开始。

## 面板健康注意（重要）

- 面板固件在"写入被拒→反复重开"的循环下会进入串口号丢失状态：
  `turzx info` 中 serial 为空。此时**多面板按串口固定路由会退化为
  单面板模式**（cmd/turzx panelPlan 已有此回退），否则两个 supervisor
  会抢同一设备（`claim interface ... bad access`）。
- 恢复手段只有物理重插（两个都拔、等 10 秒、插回）。
- 任何新方案验证前，先 `./turzx info` 确认两块屏 serial 都在。

## 候选方向（未验证）

按预期收益排序：

1. **验证流水线 + 帧拷贝**（代码已就位）。理论上限 ~25fps（采集端
   常态上限），把 USB 往返完全藏进采集等待后面。
2. **合并 probe**：把 flushLocked 的 5 次 1ms probe 改为一次
   1ms + 无数据即返回（现在 flushAttempts=5，空端点只付 1 次 1ms，
   已经接近这个形态；剩余收益小）。
3. **降低 queueDepth 轮询成本**：每帧一次 CmdStreamStatus 往返
   （~几 ms）。可改为每 N 帧一次 + 本地令牌桶推算，风险是节奏
   失准导致队列溢出（回到崩溃循环），需要小心验证。
4. **编码端**：`MaxKeyFrameInterval` 目前 = fps（每秒一个 IDR）。
   关键帧 ~9.5KB vs P 帧 ~1KB。若改为 2s 一个 IDR，平均帧减半，
   USB 时间减半 —— 代价是切屏/重连后 2 秒内画面落后。此项
   与面板解码无关，纯主机侧收益。
5. **ScreenCaptureKit 发帧节奏**：capture-only 25fps 意味着即使 USB
   完全隐藏，25fps 也是天花板。SCStreamConfiguration
   `minimumFrameInterval` 已设为 1/fps；剩余空间在
   `queueDepth`（当前 5）与 SCContentFilter 的排除列表 —— 未测。

## 测量工具（本会话用过的）

- `/tmp/animwin`：在虚拟屏上放动画窗口（真实变化内容），否则
  ScreenCaptureKit 只在内容变化时发帧，测不出真实上限。
- `turzx video <预编码.h264>`：绕过采集，测 USB+解码上限。
- 单面板验证：`--serial <serial>` 固定一块屏，排除多屏干扰。
- `--verbose` 的统计行含 `queue=`：队列恒 0 = 主机侧瓶颈；
  队列 >3 = 面板侧跟不上。


## v3 流水线验证结果（帧拷贝修复后，双面板同时）

- 两块屏同时推流 + 动画窗口，每块 **~15.5 fps 稳定**，无衰减、无会话重置、queue=0。
- 结论：帧拷贝修复了流水线的数据破坏（不再从 10 衰减到 1），但 **fps 没有提升**。
  瓶颈不在 USB 往返串行；下一步应单面板对比，并在采集/编码端（候选 4、5）找。
- 同时修了退出顺序：先等最后一次发送完成再 StopStream。

## 结论（最终）：瓶颈是测试内容，不是管线

- 之前 ~15fps 的根因是测试动画窗口（`NSView drawRect` + 定时器）自身只能重绘 ~12fps，
  ScreenCaptureKit 只在内容变化时发帧。改用 Core Animation（GPU 合成）后：
  **双面板同时 29.4 fps（目标 30），queue=0，sendwait=0ms，零重置。**
- 每帧：旋转 ~1.6ms，编码提交 ~0.2ms，USB 发送被流水线完全隐藏。
- 真实桌面（窗口拖动、视频）内容变化时即可达到 30fps；静态桌面不发帧是正常的。
- 注意：测试窗口坐标用 Cocoa（左下原点），CG 的 y=1692 对应 Cocoa y=-720。
- 默认方向改为 landscape（1920×720 虚拟屏，旋转后推给 720×1920 面板）。
- `TURZX_PROF=1` 打印回调内各阶段耗时。
