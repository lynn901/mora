# ADR-XXXX: <组件名> 引入决策

- Status: proposed | accepted | superseded | deprecated
- Date: YYYY-MM-DD
- Deciders: <角色/负责人>
- ADR-IDs: <逗号分隔的关联 ADR，如被取代填 superseded-by: ADR-YYYY>

## 背景

<为什么需要引入这个第三方组件？解决什么问题？现有方案的不足。必须说明是否触及 §16.1 "第三方治理门禁" 的触发条件：新增 / 升级 / 更换运行期第三方依赖、引入外部数据源、引入 Agent Skill 包或外部推理能力。>

## 候选对比

| 候选 | 语言/运行时 | License | 活跃度（最近提交/发版） | 维护成本 | 性能/资源 | 合规影响 |
|---|---|---|---|---|---|---|
| <候选 A> |  |  |  |  |  |  |
| <候选 B> |  |  |  |  |  |  |
| <自研/不引入> | — | — | — |  |  |  |

## 决策

<选定哪个候选，理由。若选择"不引入"，说明替代路径。>

## License 合规影响

<逐项核对：AGPL/GPL/LGPL 传染性义务是否触发？是否有"网络使用"条款（AGPL）？是否有商业使用限制 / 附加条款（commons-clause、BUSL、SSPL 等）？是否与 Mora Apache-2.0 主许可证相容？结论必须是"相容 / 不相容 / 需法务介入"。>

## 固定基线

- 组件名:
- source_url: <仓库或分发源 URL>
- commit_sha_or_digest: <Git commit SHA（源码）或不可变分发的 digest（npm integrity / Go module h1 / OCI digest）>
- version: <语义化版本或 spec 版本>
- license: <SPDX 标识，如 MIT、Apache-2.0、BSD-3-Clause>
- notice_path: `third-party/NOTICES/<组件名>.NOTICE`（组件 LICENSE/NOTICE 文件副本的相对路径，存在则校验通过）
- capability: <该组件向 Mora 暴露的能力，如"代码符号 / 调用 / 影响查询"、"Skill 包格式 profile"、"外部推理"——对应 §16.1 capability 维度>

> 固定后，任何升级 / 更换 commit / 更换 digest 都必须更新本 ADR 与 `third-party/lock.json`，并由 `make third-party-check` 校验通过。

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| <如：上游停更 / 安全漏洞 / license 变更 / 出网依赖> | <具体缓解措施> |

## 升级与回退策略

<升级触发条件、回退路径、是否需重新走 ADR。锁定到具体 commit/digest，禁止漂移到浮动 tag。>

## 门禁要求

- [ ] 已写入 `third-party/lock.json`（name / source_url / commit_sha_or_digest / version / license / notice_path / capability 齐全）。
- [ ] NOTICE 文件副本已置于 `third-party/NOTICES/`。
- [ ] `make third-party-check` 通过（digest 与 `go.sum` / `web/package-lock.json` 一致，license 在白名单，NOTICE 齐全）。
- [ ] `make sbom` 生成的 SBOM 包含该组件。
