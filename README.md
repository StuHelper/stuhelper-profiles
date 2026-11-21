---
type: index
status: Accepted
updated: 2026-11-22
---

# StuHelper Profiles

profiles 按 issuer 与 subject 保存本人昵称、头像引用、简介、公开主页 ID、展示偏好、群名片许可和业务账号注销状态。PostgreSQL 的 `profiles` schema 保存资料、修订、幂等回执和事件，迁移与运行身份分开。

## 模块边界

[module.yaml](module.yaml) 声明 platform 为必需依赖。identity、media、verification、billing、reviews、resources、favorites、open-platform、qq-binding、storage 和 storage-contribution 为可选依赖。`profiles.manage` 依赖 identity 提供当前账号。

宿主通过 verification 的公开服务取得学校与认证事实，通过 media 管理头像文件与引用，通过其他模块的公开端口处理注销影响。公开主页只返回本模块公开资料和获准展示的事实，评价与资料内容由各自模块按当前权限提供。[个人中心规格](https://github.com/StuHelper/stuhelper/blob/develop/docs/spec/personal-center.md)

## 本人资料与公开主页

首次建档依次使用 SSO 显示名和用户名中的首个合规值。两个值均不合规时，登录成功并记录待填写昵称。补填前主页不公开，后续登录保留本人保存的昵称。

昵称最多 20 个 Unicode 码点。简介先将 CRLF 转为 LF，再检查 120 个码点的上限、UTF-8 与控制字符。`InputError` 可携带字段和失败原因。公开主页 ID 与注销 ID 使用规范小写 UUID。

[OpenAPI](contracts/profiles.openapi.json) 定义 HTTP 契约。本人资料使用 `GET /api/v1/profiles/me`。同路径的 PUT 要求当前 `revision`、`Idempotency-Key` 和 `X-CSRF-Token`，冲突响应携带当前资料。幂等键为 1 至 128 个 ASCII 可见字符。

省略的简介、头像及展示偏好沿用已存值。更新展示偏好时须提交完整的六个布尔值。资料、修订、幂等结果和事件同事务写入。`POST /api/v1/profiles/me/preview` 使用草稿与当前资料合成预览，要求本人登录和 CSRF 令牌，无持久化写入。

头像使用本人已就绪的 `profile_avatar` 媒体版本，传空字符串可清除头像。激活、替换和释放按账号串行。激活前保存清理意图，资料提交时同步确认已使用的引用。保存结果未知时先核对幂等回执，未释放的引用可通过 `FlushAvatarReleases` 重试。

游客使用 `GET /api/v1/profiles/{id}` 读取已完成昵称的可见主页。学校、认证徽章、学院、署名评价和资料默认公开，年级默认关闭。接口按当前偏好过滤学校事实。事实源故障时仍可读取基础主页，`facts_state` 为 `unavailable`。

游客使用 `GET /api/v1/profiles/{id}/avatar` 读取当前公开头像。宿主通过 `PublicAvatar` 查找可见主页已选中的头像，再由 media 核验并读取对应版本。

宿主可用 `FlushPendingAvatarReleases` 分页恢复待释放引用。个别账号释放失败仍推进分页，扫描到末尾后从头重试。进程重启时从队首读取持久队列。

`PublicBatch` 和 `PublicByIDs` 是宿主批量读取端口，每次最多 100 个去重目标。仓储只返回可见账号的公开资料。事实端口若实现 `BatchPublicFacts`，可批量读取学校、认证徽章、学院和年级。

## 群名片用途许可

`group_card_consent` 在本人资料中保存七项布尔值，分别为学校、年级、院系、姓名、学号、性别和昵称。默认全部关闭，与公开主页偏好独立。PUT 省略该对象时沿用原值，提交对象时须包含七项。

`GroupCardFields` 只接受 `group_card` 用途。`GroupCardProvider` 每次核验服务身份和学校内 `profiles.group-card.read`。昵称来自 profiles，其他获准字段通过端口访问各自来源。结果用 `Allowed` 和 `Available` 表示许可与事实状态。

未获许可或缺少事实时字段值为空。来源失败或返回不合规事实时整次读取失败。撤回许可影响下一次读取，公开主页不会返回群名片许可。

## 按账号信息查询

`Service.Query` 是受信 Go 端口，接受 `kind=account`。将 issuer 与 subject 分别作无填充 base64url 编码，以冒号连接成查询值。用途必填，最多 100 条，`After` 为排他的公开 ID 游标。查询参数在格式化输出中脱敏。

`InformationAuthority` 先检查 `profiles.information-query` 入口能力，再验证目标账号在请求学校或群的范围。入口无权返回拒绝，目标越权返回空结果。授权或事实不可用时返回 `ErrUnavailable`。宿主负责按当前权限核验范围并持久记录拒绝审计。

字段白名单为 `nickname`、`avatar_version_id`、`bio`、`visibility` 和 `group_card_consent`，均为业务字段。接口拒绝敏感展开。处理中或已关闭的账号标记 `Current=false`，更新时间取最近的资料事件。[信息查询规格](https://github.com/StuHelper/stuhelper/blob/develop/docs/spec/information-query.md)

## 业务账号注销

本人提交申请时须确认积分清零规则，7 天冷静期内可以撤销。到期后，受权调用方可用 `ProcessClosure` 依次核验资产、撤销本应用会话、撤回本应用授权并处理公开内容。资产未结清的账号可继续使用，资产确认后拒绝业务访问并隐藏公开主页。

资产步骤须核对 Ivory 余额、退款和待确认支付，并原子冻结后续账务写入，清零积分与待抵扣记录。内容步骤移除公开账号关联、清理收藏与 QQ 绑定、释放头像引用，并保留公开内容本身。共享 SSO 及其他应用的账号由各自服务管理。

宿主根据持久安装历史确认从未安装的模块并记录跳过原因。资产核对涉及 billing，授权撤回涉及 open-platform、storage、storage-contribution，内容处理涉及 reviews、resources、favorites、qq-binding 和 verification。已安装模块不可用或无法核实安装历史时，申请等待处理。

各外部操作按申请 ID 幂等。结果不明时先核实执行状态，再决定重试。每步凭有效租约保存结果或释放租约。已完成的步骤在调用断开时仍尝试保存。重复处理读取当前进度，记录重试和实际操作者。

资产阻断用 `assets_pending` 表示，`pending_reasons` 可包括 Ivory 余额未清、退款处理中和支付待确认。完成注销后清空昵称、头像、简介与个人回执，重置展示偏好和群名片许可，并保存禁止自动重建的标记。

管理列表按创建时间和 ID 升序分页，`after` 为规范小写 UUID，单页最多 100 条。`QueryClosures` 支持按 `state`、`actionable`、`blocker` 筛选并返回 `next_cursor`。`ListClosureEvents` 按最新时间返回审计事件。

`DueClosures` 为管理员列出到期及待重试的注销。宿主的 `ProcessDueClosures` 每次最多处理 100 个到期或中断的申请，复用原申请与已保存步骤。遇到异常申请仍继续分页。资产或依赖阻断留在管理队列，由有权管理员续办。

宿主可用 `HasDueClosureWork` 读取积压。会话等业务依赖未装配时可用这个端口报告到期任务。

`CleanupExpired` 在 3 秒内按类别处理每批至多 1000 条回执、终态申请和注销审计。幂等回执保留 24 小时。数据库记录终态时间，满 12 个自然月后分批清事件，事件清完再删除对应申请。未完成申请与关闭标记继续保留。

迁移建立受限清理函数，宿主运行身份通过显式授权执行。过期清理以数据库当前时刻为上限，立即清理本人回执须完成注销。普通运行身份不能直接删除上述回执或注销审计。

## 验证

在已安装仓库声明的工具链与 PostgreSQL 的环境中执行 CI 使用的独立集群检查。

```bash
make setup
bash scripts/check-with-postgres.sh
```

修改 OpenAPI 后用 `make generate` 更新 Go 和 TypeScript SDK。CI 核对生成一致性，并用隔离数据库测试归属、并发、修订冲突、许可撤回、注销恢复与重复迁移。

`module.yaml` 的 `scenario_tests` 使用 `文件#函数` 登记测试。PC-05、PC-08、PC-10 覆盖公开主页、资料修改及昵称补填。PC-07、PC-09、PC-13 覆盖注销、撤销、到期推进和重启恢复。PC-12 覆盖群名片许可，IQ-02 覆盖账号查询范围。

## 生产验收与待核对

StuHelper v0.1.31 锁定 profiles v0.1.1，生产账号页面已通过匿名访问核验。宿主正式接入草稿预览、头像、注销扫描及清理端口的范围需按后续组合核对。注销和公开主页的真实账号操作待验收。[正式组合](https://github.com/StuHelper/stuhelper/releases/tag/v0.1.31)
