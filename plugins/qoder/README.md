# Qoder CPA 插件

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 Qoder Provider 插件，一个插件覆盖 CN（`qoder.com.cn`）与 Intl（`qoder.com`）双区。安装、配置与架构说明见[仓库根目录 README](../../README.md)。

## 0.9.24 可靠性更新

- 签到结果以 Qoder 回执/状态确认，手动、自动、重试共用锁；最多三次有界重试，不增加签到记录界面。
- 统一代理路由；桥接失败不再直连重放，登录和聊天遵守相同路由。
- 登录任务及 token 检查点持久化，保存失败可恢复，不重复交换已有 token。
- 上下文覆盖同步请求和 CPA 声明，按实际账号档位校验，不借用其他账号能力。
- 配额作用域与恢复条件可解释，保留人工禁用，缓存合并保护新确认和余额。

详见[功能与限制](../../docs/reliability.md)和[模型契约](../../docs/qoder-model-contract.md)。

## 构建

```bash
make build          # 当前平台 → qoder.so
make test           # go test -race
make lint           # gofmt + go vet
make release        # 交叉编译 linux/amd64、linux/arm64
```

## 关键源码

| 文件 | 说明 |
|---|---|
| `main.go` | 插件注册、执行器入口、上游常量 |
| `oauth.go` / `region.go` | 双区设备码登录与按账号区域路由 |
| `billing.go` / `checkin.go` | 额度查询、签到调度与生命周期 |
| `campaign.go` | Intl campaign 签到契约 |
| `models.go` / `model_config.go` / `model_order.go` | 模型目录与面板 overlay |
| `cooldown.go` | 按 (账号, 模型) 维度的冷却 |
| `panel.html` | 内嵌管理面板 |
