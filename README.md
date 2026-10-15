# frpc-gui

[frp](https://github.com/fatedier/frp) 客户端的图形界面启动器。填写服务器 IP 和 Token 即可启动内网穿透，支持配置多条 TCP 代理。

## 特点

- 零第三方依赖 —— 纯 `syscall` 调用 Win32 API，不用任何 GUI 库
- 体积小，单文件约 2.9 MB，无控制台窗口
- 支持高 DPI 屏幕，125% / 150% 缩放下界面清晰不模糊
- 多代理管理：添加、修改、删除、上移、下移，重名自动补全
- 三种方式指定 frpc.exe：自动探测 / 浏览选择 / 拖拽到窗口

## 使用方法

### 1. 下载 frpc.exe

到 [frp 官方 Releases](https://github.com/fatedier/frp/releases) 下载对应平台的压缩包，解出 `frpc.exe`。

### 2. 与本程序放在同一目录

```
frpc-gui.exe      ← 本程序
frpc.exe          ← 官方 frp 客户端
frpc-gui-data/    ← 首次运行自动生成，存放配置
```

### 3. 运行

双击 `frpc-gui.exe`：

1. 填写服务器 IP、端口（默认 7000）、Token
2. 点「添加」配置代理映射（本地端口 → 远程端口）
3. 点「保存并启动」

配置会保存在同目录的 `frpc-gui-data/config.json`，下次打开自动回填。

## 功能

| 功能 | 说明 |
| --- | --- |
| 服务器设置 | IP、端口、Token，Token 可切换明文显示 |
| 代理列表 | 添加 / 修改 / 删除 / 上移 / 下移 |
| frpc.exe 定位 | 自动探测、浏览选择、拖拽入窗 |
| 运行日志 | 实时显示 frpc 输出，Token 自动打码 |
| 配置持久化 | 下次启动自动回填上次的设置 |

## 为什么需要两个文件

本程序**不内嵌** frpc.exe。

内嵌后程序需要在运行时把可执行文件释放到磁盘再调用，这种「释放并执行」的行为与木马释放器特征一致，容易被杀毒软件误报拦截。因此本程序只负责生成配置并调用你指定的官方 frpc.exe。

## 构建

需要 Go 1.21 或更高版本：

```bash
go build -trimpath -ldflags "-H windowsgui -s -w" -o frpc-gui.exe .
```

- `-trimpath` 去掉二进制中的本机源码路径
- `-H windowsgui` 去掉控制台窗口
- `-s -w` 去掉符号表和调试信息

运行测试：

```bash
go test ./...
```

## 许可

GUI 部分暂无许可证。调用的 [frp](https://github.com/fatedier/frp) 采用 Apache-2.0 许可。
