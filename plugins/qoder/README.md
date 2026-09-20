# Qoder CPA 插件

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 Qoder Provider 插件，一个插件覆盖 CN（`qoder.com.cn`）与 Intl（`qoder.com`）双区。安装、配置与架构说明见[仓库根目录 README](../../README.md)。

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
