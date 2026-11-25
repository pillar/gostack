# 安全说明

本文件描述已实现的安全机制及其边界。未列出的能力视为未实现。

## 凭据与密钥

- API Key / GameKit 会话令牌：256 位随机值，仅存 SHA-256 摘要；撤销直接查库立即生效（`core/auth`）。
- 配置中的密钥只能是引用（`env:NAME` / `file:/path`），配置校验拒绝明文；日志对含 secret/token/password/cookie 等字段名的属性脱敏。
- systemd：独立低权限用户、`NoNewPrivileges`、`ProtectSystem=strict`、仅 `/var/lib/APP` 可写；配置与环境文件 0640 root:APP。

## Web 认证（`modules/webauth`，可选，`auth.web.enabled`，默认关闭）

接口见 `docs/api/webauth.md`。

| 方面 | 实现 |
|---|---|
| 密码哈希 | Argon2id（`golang.org/x/crypto/argon2`），PHC 字符串 `$argon2id$v=19$m=19456,t=2,p=1$salt$hash`；参数取 OWASP Password Storage Cheat Sheet 的最低推荐（19 MiB、2 次迭代、1 并行度），16 字节盐、32 字节输出。验证从存储串读取参数，日后提高参数不影响旧哈希（目前不会在登录时自动重新哈希）。 |
| 内存保护 | 同时进行的 Argon2id 计算数受 `auth.web.max_concurrent_hashes`（默认 2，≈38 MiB）限制，超出时排队至请求超时后返回 503。 |
| 密码规则 | 8..128 **字节**（UTF-8 多字节字符按字节计）；不做 Unicode 规范化、不查泄露库。 |
| 邮箱 | 去空白、转小写后唯一（每租户）；简单格式检查，不验证邮箱所有权。 |
| 错误一致性 | 账号不存在、密码错误、密码长度非法均返回同一 `401 invalid_credentials`；不存在的账号也会对一个虚拟哈希执行一次 Argon2id，避免计时枚举（测试比较最小耗时）。 |
| 枚举边界 | **注册接口**对已存在邮箱返回 `409 email_taken`，可被用于判断邮箱是否注册（受每 IP 限流约束）。需要防枚举时关闭 `allow_signup`，由应用实现带邮箱验证的注册。 |
| 限流 | 登录/注册按 IP（IPv6 按 /64）`ip_rate_per_minute`，登录与改密按账号 `account_rate_per_minute`；进程内令牌桶（多实例时限额相乘）；profile scale 设 `rate_limit.driver: valkey` 时为跨实例全局限流，Valkey 故障时退化为每实例令牌桶（ADR-0004）；均可热更新。"IP" 取自直连对端；只有对端属于 `http.trusted_proxies` 时才解析 `X-Forwarded-For`（从右向左跳过受信代理），`CF-Connecting-IP` 仅在 `http.trust_cf_connecting_ip: true`（默认 false，仅当所有受信代理都是 Cloudflare 时开启）时采用，否则客户端可伪造该头绕过每 IP 限流。按账号限流可被他人消耗导致该账号短时无法登录（锁定型 DoS），这是有意的权衡。 |
| 会话 | 256 位不透明令牌（`gws_` 前缀），库内只存 SHA-256；绝对有效期 `session_ttl`（默认 7 天）+ 空闲超时 `idle_timeout`（默认 24h，最近使用时间至多每 idle/10 写一次）；登录时作废浏览器带来的旧会话（防会话固定）；登出删除会话。 |
| Cookie | `HttpOnly; SameSite=Lax; Path=/`。`environment: production` 时加 `Secure` 且名称为 `__Host-gostack_session`（仅限当前主机、必须 HTTPS）；开发环境名称 `gostack_session`、无 Secure。 |
| CSRF | 非安全方法（非 GET/HEAD/OPTIONS）需要 `X-CSRF-Token` 等于会话的 CSRF 令牌（登录/注册/me 返回，常量时间比较）；同时检查 `Origin`（须与请求 Host 相同，生产必须 https，或在 `allowed_origins` 中）与 `Sec-Fetch-Site`（仅 `same-origin`/`none`，或来源在白名单）。登录与注册也做来源检查（防登录 CSRF）。请求体必须是 `application/json`。无 Origin 与 Sec-Fetch-Site 的非浏览器请求通过来源检查，但仍需 CSRF 令牌。 |
| 改密 | 需当前密码 + CSRF；成功后删除该用户**全部**会话并为当前浏览器签发新会话。 |
| 跨源 | `allowed_origins` 中的来源获得带凭据的 CORS 响应（仅 `/v1/auth/*` 及使用 `Module.CORS` 包装的路由）。SameSite=Lax 下跨站（非同站）请求不会携带 Cookie，跨站前端不受支持。 |
| 租户 | 注册用户进入 `Module.Tenant`（默认 `default`）；受保护路由的租户与用户只来自会话（`webauth.SessionFrom`），不信任请求参数。 |

应用保护自己的路由：
```go
mux.Handle("POST /v1/notes", a.WebAuth.Require(handler))   // 401 未登录；403 CSRF/来源失败
s, _ := webauth.SessionFrom(r.Context())                    // s.TenantID, s.UserID
```

未实现：邮箱验证、找回密码、MFA、账号锁定通知、会话列表/远程登出 API、按客户端绑定会话。

## 部署与供应链

- 发布物带 sha256（`SHA256SUMS`）与构建信息 SBOM（非 SPDX，见 `docs/DEPLOY.md`）；部署脚本拒绝未提供或不匹配校验和的二进制，版本号不可复用于不同内容。
- 依赖：`modernc.org/sqlite`、`go.yaml.in/yaml/v3`、`golang.org/x/crypto`（仅 argon2）。govulncheck 在 CI 运行。
- 回滚脚本拒绝在比目标二进制更新的 schema 上运行。

## 未执行的安全验收

系统性安全测试（A07 中的渗透/模糊测试）、真实 Linux 主机上的部署权限验证均未执行。
