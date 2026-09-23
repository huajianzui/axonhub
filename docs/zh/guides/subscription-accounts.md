# 订阅账号

订阅型渠道（Codex、Claude、Antigravity、xAI）使用 OAuth 授权而不是 API Key。本指南介绍如何在一个渠道中管理多个订阅账号。

## 什么是订阅账号？

**一个订阅账号 = 一份 OAuth 授权。** 一个渠道可以持有多个账号，请求会在它们之间分配。

这解决了什么：

| 没有多账号 | 有多账号 |
|-----------|---------|
| 一个订阅用完了就得换 | 把多个订阅放在同一渠道里，自动分配 |
| 一个账号触发限流，整个渠道不可用 | 只有那个账号淡出，其他照常服务 |
| 加一个订阅要新建渠道、重配模型 | 在同一个渠道里加一个账号即可 |

## 账号从哪来

**已有渠道会自动迁移。** 升级后，每个原本内联保存 OAuth 凭据的订阅渠道会自动生成一个账号，渠道自身的凭据保留不动——所以升级不会影响正在服务的渠道，回退也是安全的。

新增的账号来自你完成的授权流程。

## 添加账号

1. 进入 **渠道管理**，在目标渠道所在行打开右侧菜单（⋮）
2. 选择 **账号管理**
3. 点击 **添加账号**
4. 在 **凭据** 中粘贴授权流程返回的结果
5. 可选填 **备注**（一般写账号邮箱，方便区分）
6. 点击 **添加**

### 凭据从哪来

先在渠道编辑弹窗里完成一次 OAuth 授权（`开始 OAuth 授权`）。这一步会产出一段凭据：

- **Codex / Claude Code / xAI**：一段 JSON，形如 `{"access_token":"...","refresh_token":"..."}`
- **Antigravity**：形如 `<refreshToken>|<projectID>`

把这段结果粘贴到账号弹窗即可。渠道类型决定服务端如何解析它，不需要你指定格式。

> 重复添加同一份凭据不会产生重复账号——会复用已有账号。

## 账号状态

| 状态 | 含义 |
|------|------|
| **正常** | 可以服务请求 |
| **刷新中** | 正在换取新的访问令牌 |
| **需重新授权** | 上游拒绝了这份授权，需要重新授权 |
| **状态未知** | 刷新结果不确定，系统不会猜测 |

另有独立的 **已暂停** 标记：由你手动控制，与授权状态互不影响。暂停的账号保留凭据和记录，可以随时恢复。

**只有「正常」且未暂停的账号会收到请求。**

## 调度行为

- 请求按账号**权重**分配，权重高者优先
- **同一会话会固定使用其起始账号**，因此调整权重不会打断正在进行的对话（这一点对上游提示词缓存很重要）
- 账号出现鉴权失败时**只暂停该账号**；只有当渠道下**再无可用账号**时，渠道才会被禁用
- 删除账号不会让渠道失去服务能力——删掉最后一个账号后，渠道会回退到自身凭据

## 常见问题

**问：添加账号后为什么没有立即生效？**

渠道的账号集合在渠道缓存重建时载入。控制台在增删改账号时会自动触发重建，通常无需干预；若未生效，编辑并保存该渠道即可强制重建。

**问：一个账号被禁用后如何恢复？**

在账号列表中把该账号重新启用。若账号处于「需重新授权」，需要在渠道弹窗里重新完成一次授权，再添加账号。

**问：为什么看不到凭据？**

凭据从不通过 API 返回。控制台只读取身份、状态、权重和过期时间。

**问：Antigravity 的多账号有什么限制？**

Antigravity 的账号选择正常可用，但**令牌刷新仍是渠道级**，而不是每账号独立。需要每账号独立刷新必须修改其转换器（它目前把凭据作为字符串在构建时创建令牌提供者）。其余三类订阅渠道均为每账号独立刷新。

---

# 面向维护者

以下约束不是风格偏好，而是**违反就会出现难以定位的故障**的设计前提。

## 一、凭据指纹必须取自稳定身份

账号的每渠道唯一性建立在 `credential_fingerprint` 上，它**必须**由授权中稳定的部分计算——刷新令牌，或没有刷新令牌时的旧式凭据字符串。

原因是两条：

1. **解析会改写凭据。** 上游没返回过期时间时，解析会补一个当前时间，所以同一份授权解析两次得到的序列化结果**不同**。若指纹取自整个序列化凭据，重试导入会被误判为新账号。
2. **访问令牌每次刷新都会更换。** 若指纹包含访问令牌，刷新后的账号会被误判为另一个账号。

见 `internal/objects/account_fingerprint.go`。

## 二、渠道缓存只在渠道行变动时重建

`reloadEnabledChannels` 在渠道行 `updated_at` 未前进时**直接返回**：

```go
} else if !latestUpdatedChannel.UpdatedAt.After(lastUpdate) {
    return current, lastUpdate, false, nil
}
```

账号在**独立表**里，增删改账号**不会**触碰渠道行。因此**任何账号写操作都必须显式触碰渠道行**（`ChannelAccountService.TouchChannel`），否则运行时看不到变化。

## 三、请求上下文是共享可变容器

`contexts.WithChannelAPIKey` 写入的是一个**共享容器**（带互斥锁），而不是普通 context 值。当请求已经带有容器时，写入对所有共享者可见——生产路径上 `orchestrator` 与 trace 中间件都会安装容器。

但 `oauth.TokenGetter` 的 context 参数是**按值传递**的，接口无法把派生 context 交还调用方。所以 getter 在记录选择前**必须**先 `contexts.EnsureContainer`，否则这个记录会丢失，故障会被归因到整个渠道——**导致一个账号的失败禁用渠道下所有账号**。

## 四、新增 ent 实体必须人工补齐三处

实体生成后，以下三处**不会**自动完成，且症状具有误导性：

1. **`channelAccounts` 查询的解析器**是 `panic("not implemented")` 桩，需手写实现（`internal/server/gql/ent.resolvers.go`）。
2. **`id` 与 `channelID` 字段解析器**也是桩，需手写返回 `objects.GUID`。
3. **`guidTypeToNodeType`**（`internal/server/gql/graphql.go`）是**手写白名单**。遗漏的症状极易误判：**同一对象的其他字段正常返回，只有 `id` 报 `unknown node type`**。

已有测试 `internal/server/gql/graphql_node_types_test.go` 钉住第三处。

## 五、权限边界

- `ChannelAccount.credentials` 标记为 `Sensitive()`，因此**不会**出现在 GraphQL schema 中。这是凭据不外泄的保证，不要为了图方便给它加 GraphQL 字段。
- 实体的读写权限沿用渠道的 scope（`read_channels` / `write_channels`）。
- 账号写入端点位于 REST（`/admin/channels/:id/accounts`），因为新增账号接收的是授权流程返回的不透明字符串，这属于 REST 形状而非 GraphQL 输入。

## 六、当前已知缺口

| 缺口 | 影响 | 需要的改动 |
|------|------|-----------|
| Antigravity 刷新为渠道级 | 该类渠道无法每账号独立刷新 | 修改 `llm/transformer/antigravity`，使其按请求解析凭据而非在构建时创建提供者 |
| 账号权重仅在重载时排序 | 单账号渠道行为不变；多账号时高权重账号优先 | 无需改动，但需知晓 |

## 关键文件

| 文件 | 职责 |
|------|------|
| `internal/ent/schema/channel_account.go` | 实体与索引定义 |
| `internal/ent/migrate/datamigrate/v1.0.0-beta11.go` | 从内联凭据回填账号 |
| `internal/objects/account_credential.go` | 凭据形状的双向投影 |
| `internal/objects/account_fingerprint.go` | 稳定指纹 |
| `internal/server/biz/channel_account.go` | 账号服务、可用账号查询、投影 |
| `internal/server/biz/channel_account_token.go` | 按请求选账号 + 每账号刷新 |
| `internal/server/biz/channel_account_create.go` | 从凭据新增账号 |
| `internal/server/api/channel_account.go` | REST 写入端点 |
