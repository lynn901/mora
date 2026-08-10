# ADR-XXXX: &lt;组件&gt; 引入决策

- Status: proposed | accepted | superseded
- Date: YYYY-MM-DD
- Component: &lt;name&gt;
- Capability: &lt;该组件为 Mora 提供的能力&gt;
- Lockfile entry: `third-party/lock.json` → `components[name=<name>]`
- NOTICE: `third-party/NOTICES/<name>.<license>.txt`

## 背景

- 为什么需要引入这个组件？解决什么问题？
- 触发本 ADR 的 issue / Phase / 决策书章节。
- 与 Mora「100% 私有化、默认不出网」边界的关系。

## 候选对比

| 候选 | 语言 | License | 活跃度 | 维护成本 | 性能 | 合规影响 |
|------|------|---------|--------|----------|------|----------|
|      |      |         |        |          |      |          |

## 决策

- 选定哪个候选？为什么？
- 接入形态：Go 库 / 独立 sidecar / spec 参考 / npm 包。
- 数据流：Mora 控制面保留什么？组件拿到什么（短期凭证 + 已裁剪 `AuthzContext`，不持有 Mora 凭据）。

## License 合规影响（AGPL/GPL 传染性义务 / 商业限制）

- License SPyX：是否在 `lock.json` 的 `allowlist` / `review_required` / `denylist`？
- 是否触发 AGPL/GPL 传染性义务或商业限制？
- NOTICE 保留义务如何满足？

## 固定基线

- source_url:
- commit_sha_or_digest:
- digest_algorithm:
- license:
- notice_path:
- 何时固定（Phase / issue）：

## 风险与缓解

- 上游废弃 / 突然改 license / 被收购的风险。
- 性能与资源占用（sidecar 形态尤其）。
- 安全：组件是否执行不可信输入？是否需要沙箱？

## 升级与回退策略

- 升级条件（安全补丁 / 破坏性变更）。
- 回退路径：lock.json 回滚到上一个固定 digest；ADR `superseded`。
- 升级必须重新走本 ADR + `make third-party-check`。
