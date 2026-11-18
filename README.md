# StuHelper Profiles

profiles 拥有公开昵称、简介、公开主页 ID 和六项展示偏好，按 issuer 与 subject 保留归属。昵称最多 20 个 Unicode 码点，简介最多 120 个。账号注销编排已实现，头像引用尚未实现。

首次登录依次尝试合规的 SSO 显示名、用户名。两者都不合规时保存待填写昵称状态，登录成功，基础主页暂不公开。本人完成昵称后可以读取公开主页，后续 SSO 登录不覆盖用户修改。

学校、认证徽章、学院、署名评价和资料默认公开，年级默认不公开。六项都可关闭。公共接口通过 verification 读取当前学校事实，并再次按偏好过滤字段。事实服务故障时保留基础主页，facts_state 标明不可用。接口不返回关联内容、身份材料、绑定、账户标识或资产数据。

PublicBatch 与 PublicByIDs 支持单次最多 100 个去重目标的批量公开资料读取，仅返回有效且已完成昵称填写的公开主页，并支持通过 BatchPublicFacts 批量读取学校与认证徽章事实。

独立契约由 contracts/profiles.openapi.json 定义，执行 make generate 更新 Go 与 TypeScript SDK。GET /api/v1/profiles/me 读取本人资料，PUT 与注销 POST 接口均要求幂等键与 X-CSRF-Token。GET /api/v1/profiles/{id} 对游客开放，冲突返回当前资料。 省略的简介和展示偏好保留原值。

POST /api/v1/profiles/me/preview 接收未保存的昵称、简介与展示偏好草稿，按与公开主页相同的事实过滤规则只读合成预览结果，不写入数据库且无需幂等键。

PostgreSQL 的 profiles schema 使用独立迁移身份，运行身份只访问自己的业务表。公开 ID 与注销 ID 严格校验规范小写 UUID，幂等键要求 1–128 个可打印 ASCII 字符，简介拒绝双向文本覆盖控制符。

昵称最多 20 个 Unicode 码点。简介将 CRLF 换行转为 LF 后校验，最多 120 个码点。InputError 可返回字段名与校验失败原因。

执行 make check 验证。数据库测试使用 STUHELPER_PROFILES_TEST_DSN 指定的独立数据库，覆盖归属、并发重试、公开字段、昵称补填、Unicode 边界、偏好与重复迁移。本模块不保存登录令牌、实名材料、手机号或学校资格。

注销申请需要明确确认积分清零政策，7 天内可撤销，截止时刻起由当前管理员处理。资产核实前保留账号使用能力，资产结清后 Service.Get、修改与建档均返回 ErrClosed 并隐藏公开主页。全部步骤完成后重置展示偏好与群名片许可，清理历史资料及本人注销幂等快照并写入 closed 事件，同一 SSO 身份不能自动重建。

流程持久保存申请、管理员批准、各步骤与失败原因。依赖故障保留进度，同一申请再次处理时从未完成步骤继续。各外部端口必须按申请 ID 幂等，并先核实结果不明的写入。管理列表按 (created_at, id) 升序分页，使用规范小写 UUID 游标 after 与 limit（最多 100 条）。

管理列表支持按 state、actionable、blocker 筛选并返回 next_cursor。ListClosureEvents 按时间倒序返回只读注销审计记录。

资产端口必须确认余额、退款及待确认支付已处理，原子冻结后续账务写入并清零所有积分及待抵扣记录。会话和授权端口仅处理本应用，内容端口匿名保留正文与文件。装配方只有在持久安装历史明确证明账务或开放平台从未安装时，才跳过对应步骤。曾安装但关闭、历史缺失或不可读时继续等待，不根据启用配置推断。

资产、授权和内容步骤分别返回跳过的模块，资产仅接受 billing，授权仅接受 open-platform，内容接受 reviews、resources、favorites、qq-binding。模块名与步骤不匹配时拒绝写入。

注销结果通过 skipped_modules 保存经持久安装历史核实后跳过的模块。ProcessClosure 通过短租约互斥步骤执行，客户端断连时仍保存已完成步骤进度，幂等重放回查最新状态，恢复重试记录 retry 与实际操作管理员。

资产未结清返回 assets_pending 时，Closure.pending_reasons 可携带脱敏子原因（Ivory 余额未清、退款处理中、待确认支付），不含具体金额，资产结清后自动清空。ClosureRepository 与 Service 预留 DueClosures 按时间升序扫描冷静期满及待重试注销单。

CleanupExpired 按 1–1000 的批量上限与 3 秒超时清理满 24 小时的幂等响应，以及满 12 个自然月的已关闭或已撤销注销记录与关联事件，未完成注销单与已关闭账号标记继续保留。

业务账号注销通过 qq-binding 公开接口删除三项绑定并关闭后续绑定写入。模块曾安装但暂不可用时等待恢复，只有安装历史明确证明从未安装时才跳过。

## 群名片用途许可

GET /api/v1/profiles/me 的 Profile 新增 group_card_consent，PUT 的 ProfileUpdate 可传同名对象。对象必须包含 school、grade、department、name、student_id、gender、nickname 七个布尔值。默认全 false，省略对象保留原值，与公开主页偏好独立。

修改沿用 revision 和 Idempotency-Key，许可、资料修订、幂等结果与 profiles.events 在同一事务保存。迁移给已有资料添加默认拒绝许可，不覆盖后续设置。公开主页不返回许可。

Service.GroupCardFields(ctx, principal, owner, school, purpose) 返回 map[string]CardField。purpose 仅接受 group_card。每项含 Allowed、Available、Value，拒绝或缺失时 Value 为空。

nickname 来自本模块；其余字段按固定顺序经 GroupCards 端口读取获准来源，并拒绝空值、超长或含控制符与双向覆盖符的事实。

backend 注入 GroupCardProvider：AuthorizeGroupCard 每次验证当前服务身份及学校内 profiles.group-card.read；GroupCardValues 将许可字段转交 verification.GroupCardValues，并转换 Available/Value。不得从请求载荷授予服务能力，撤权后调用立即拒绝。

端口缺失返回 ErrUnavailable，缺失资料返回 ErrNotFound，关闭账号返回 ErrClosed，其他用途或非法 owner 返回 ErrInput，无主体返回 ErrUnauthenticated，学校越权返回 ErrForbidden。来源错误保留，backend 负责按提供方类型映射；失败返回 nil，不使用部分事实。

Go/TypeScript SDK 已按更新后的 OpenAPI 生成。新增 PC-12 数据库测试覆盖许可修改与撤回、字段缺失、学校和服务授权、事件失败回滚及重复迁移。backend 固定本模块与 verification 提交后接入，前端设置控件由客户端任务实现。

## 信息查询端口

提供方使用本包的 Principal、Scope、Query、Record 与 Field，backend 负责转换信息查询 DTO。Query 仅接受 kind=account，Value 是 issuer 与 subject 分别做 base64url 无填充编码后以冒号连接的完整账号键。

`Service.Query(ctx context.Context, principal Principal, q Query) ([]Record, error)` 按 ID 升序返回记录。Limit 为 1–100，After 为排他游标，Fields 按字段名筛选。Purpose 必填，查询值不写日志，Query 的格式化输出脱敏。

宿主注入 InformationAuthority，逐次验证当前能力与目标学校、群范围。空目标仅检查入口能力，具体目标必须核验范围事实。授权前不解密目标资料，过滤范围后才计入分页。缺少能力返回 ErrForbidden，依赖故障返回 ErrUnavailable。

InformationAuthority.AuthorizeInformation 负责拒绝审计。AuditInformation 必须成功持久保存敏感查看审计后才能返回展开字段。请求 ID、渠道与群从可信 context 取得，审计记录用途、内部目标 ID、范围、类别、权限依据、结果和数量，不保存查询值。

OpenAPI 路径与生成 SDK 无变化。这些方法供可信 Go 装配使用，backend 接入需固定提供方提交，注入权限与审计实现，并保持查询入口自身的授权检查。

唯一查询能力为 profiles.information-query。Query 在鉴权前校验字段名白名单，返回 nickname、bio、visibility、group_card_consent，处理中或已关闭账号标记 Current=false，ChangedAt 来自最近一次资料事件（无事件时为零值）。不提供线索解析，不接受敏感字段展开。

公开设置与群名片许可是资料查询字段，群名片许可本身不授予管理员信息查询权。账号没有学校归属字段，宿主授权实现必须通过公开事实核验目标在请求学校或群的范围，不能把请求携带的 Scope 当作授权。
