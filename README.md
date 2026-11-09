# StuHelper Profiles

profiles 拥有公开昵称、简介、公开主页 ID 和六项展示偏好，按 issuer 与 subject 保留归属。昵称最多 20 个 Unicode 码点，简介最多 120 个。账号注销编排已实现，头像引用尚未实现。

首次登录依次尝试合规的 SSO 显示名、用户名。两者都不合规时保存待填写昵称状态，登录成功，基础主页暂不公开。本人完成昵称后可以读取公开主页，后续 SSO 登录不覆盖用户修改。

学校、认证徽章、学院、署名评价和资料默认公开，年级默认不公开。六项都可关闭。公共接口通过 verification 读取当前学校事实，并再次按偏好过滤字段。事实服务故障时保留基础主页，facts_state 标明不可用。接口不返回关联内容、身份材料、绑定、账户标识或资产数据。

独立接口契约由 contracts/profiles.openapi.json 定义，执行 make generate 更新本模块 Go 与 TypeScript SDK。GET /api/v1/profiles/me 读取本人资料，PUT 使用修订号和幂等键修改。GET /api/v1/profiles/{id} 对游客返回基础主页。发生冲突时返回当前资料，简介和偏好省略时保持原值。

PostgreSQL 的 profiles schema 使用独立迁移身份，运行身份只访问自己的业务表。公开 ID 稳定唯一，昵称限制为 20 个 Unicode 码点，简介限制为 120 个码点。

执行 make check 验证。数据库测试使用 STUHELPER_PROFILES_TEST_DSN 指定的独立数据库，覆盖归属、并发重试、公开字段、昵称补填、Unicode 边界、偏好与重复迁移。本模块不保存登录令牌、实名材料、手机号或学校资格。

注销申请需要明确确认积分清零政策，7 天内可撤销，截止时刻起由当前管理员处理。资产核实前保留账号使用能力，资产结清后禁止普通操作并隐藏公开主页。会话、授权和内容步骤完成后关闭业务账号，同一 SSO 身份不能自动重建。

流程持久保存申请、管理员批准、各步骤与失败原因。依赖故障保留进度，同一申请再次处理时从未完成步骤继续。各外部端口必须按申请 ID 幂等，并先核实结果不明的写入。管理列表使用 after 与 limit，每页最多 100 条。

资产端口必须确认余额、退款及待确认支付已处理，原子冻结后续账务写入并清零所有积分及待抵扣记录。会话和授权端口仅处理本应用，内容端口匿名保留正文与文件。端口缺失不会视为成功。当前后端账务真实来源尚未接入，授权步骤等待开放平台模块。

注销结果通过 skipped_modules 保存经持久安装历史核实后跳过的模块，失败与重试都保留进度。当前恢复方式为管理员通过原申请 ID 再次处理，每次校验当前管理权限。管理列表展示 blocker 和 completed_steps，依赖恢复后沿原步骤继续，不自动重新执行资产处置。

业务账号注销通过 qq-binding 公开接口删除三项绑定并关闭后续绑定写入。模块曾安装但暂不可用时等待恢复，只有安装历史明确证明从未安装时才跳过。

## 群名片用途许可

GET /api/v1/profiles/me 的 Profile 新增 group_card_consent，PUT 的 ProfileUpdate 可传同名对象。对象必须包含 school、grade、department、name、student_id、gender、nickname 七个布尔值。默认全 false，省略对象保留原值，与公开主页偏好独立。

修改沿用 revision 和 Idempotency-Key，许可、资料修订、幂等结果与 profiles.events 在同一事务保存。迁移给已有资料添加默认拒绝许可，不覆盖后续设置。公开主页不返回许可。

Service.GroupCardFields(ctx, principal, owner, school, purpose) 返回 map[string]CardField。purpose 仅接受 group_card。每项含 Allowed、Available、Value，拒绝或缺失时 Value 为空。

nickname 来自本模块；其余字段由 GroupCards 端口读取获准来源。

backend 注入 GroupCardProvider：AuthorizeGroupCard 每次验证当前服务身份及学校内 profiles.group-card.read；GroupCardValues 将许可字段转交 verification.GroupCardValues，并转换 Available/Value。不得从请求载荷授予服务能力，撤权后调用立即拒绝。

端口缺失返回 ErrUnavailable，缺失资料返回 ErrNotFound，关闭账号返回 ErrClosed，其他用途或非法 owner 返回 ErrInput，无主体返回 ErrUnauthenticated，学校越权返回 ErrForbidden。来源错误保留，backend 负责按提供方类型映射；失败返回 nil，不使用部分事实。

Go/TypeScript SDK 已按更新后的 OpenAPI 生成。新增 PC-12 数据库测试覆盖许可修改与撤回、字段缺失、学校和服务授权、事件失败回滚及重复迁移。backend 固定本模块与 verification 提交后接入，前端设置控件由客户端任务实现。
