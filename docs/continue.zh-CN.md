# 自动续跑挂起的任务

[English](continue.md) | **中文**

返回[中文 README](../README.zh-CN.md) · [文档目录](README.zh-CN.md)。

`watch` 和 `bg` 能让窗口链保持接龙,但它们不会去恢复一个已经停在 5h 限额处的任务。
`limitping continue <provider>` 可以:它通过 PTY 启动 Provider 真正的交互式 CLI,把你的
终端原样透传,你照常使用 Codex / Claude Code。它在后台监测用量,当 5h 限额(曾打满)
恢复的瞬间,把续跑消息输入到会话里,让长任务自己接着跑,而不是一直停在限额处等你回来。

```sh
limitping continue codex                       # 照常使用 Codex;恢复时自动续跑
limitping continue codex --yolo                # Provider 后面的参数原样透传给 CLI
limitping continue claude --dangerously-skip-permissions
```

- 续跑消息取配置中各 Provider 的 `continue_prompt`(默认 `"continue"`,可改成如
  `"继续任务"`)。退出请用该 CLI 自带的退出方式。
- 只在真正的恢复边沿注入:5h 窗口曾打满(或端点报告 `limit_reached`,或 CLI 打印过
  限额消息)且随后明确重置,同时周窗口也未耗尽(按 `weekly_threshold`,含 credits)——
  因此不会一恢复就直接撞上周墙。
- 新版 Claude Code 能在限额重置后自己续跑任务(`/config` 中的 "Continue automatically at
  usage limit",默认开启)。`continue claude` 会让它先来:恢复时最多等 90 秒,看 Claude Code
  是否宣布续跑或已开始这一轮,只有没有时才输入续跑消息 —— 任务不会被续两次,旧版
  Claude Code 也照样能续上。
- 诊断时间线写入 `~/.config/limitping/continue.log`(超过 1 MB 轮转为 `continue.log.1`)。
- 支持 Unix(PTY)和 Windows 10 1809+(ConPTY)。Windows 说明:
  - Ctrl+Z 不会转发给 CLI(控制台把它当作输入结束,`continue` 会忽略它,键盘输入照常可用)。
  - 关闭控制台窗口或按 Ctrl+Break 会立即强制结束 CLI 及其子进程,CLI 没有机会保存会话状态;
    需要保存时请正常退出 CLI(如 `/exit`)。Ctrl+C 与 Unix 一样作为按键传给 CLI。
  - 通过 npm 安装的 `claude`(`.cmd` 垫片)用于判断 Claude Code 是否已自行续跑的进程 ID
    只能尽力识别;识别不了时 `continue` 仅依据屏幕提示和 90 秒宽限,偶尔可能在 Claude Code
    已续跑的任务上重复输入续跑消息。
  - 你自己的输入不会插进注入的消息中间,但注入前已在提示框里输入的文字不受保护。
