// Phase 5-4 — validation & compatibility report visualization (§4.3 / §8).
//
// Renders what the §6.1 endpoints return — never reads skill_packages
// directly (architecture red line §4.4 + §9). No secret values are ever
// shown: signature is presence/shape only (§1.2); provenance carries only
// a reference, never plaintext credentials.
import { ShieldCheck, FileWarning, Boxes } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Separator } from "@/components/ui/separator"
import {
  DeliveryVerdictBadge,
  SeverityBadge,
  ValidationStatusBadge,
} from "./binding-primitives"
import { fmtTime } from "./binding-utils"
import type {
  CompatibilityReport,
  SkillPackage,
  ValidationReport,
} from "@/types/binding"

/** §4.3 validation_report.findings — each finding with severity + code. */
function FindingsList({ report }: { report: ValidationReport }) {
  const findings = report.findings ?? []
  if (findings.length === 0) {
    return (
      <p className="text-xs text-muted-foreground">无校验发现项。</p>
    )
  }
  return (
    <ul className="space-y-1.5">
      {findings.map((f, i) => (
        <li
          key={`${f.check}-${i}`}
          className="rounded-md border bg-muted/30 p-2 text-xs"
        >
          <div className="flex flex-wrap items-center gap-2">
            <SeverityBadge status={f.severity} />
            <code className="rounded bg-muted px-1 py-0.5 text-[10px] font-mono">
              {f.code}
            </code>
            <span className="text-muted-foreground">{f.check}</span>
          </div>
          <p className="mt-1 text-foreground">{f.message}</p>
          {f.path && (
            <p className="mt-0.5 font-mono text-[10px] text-muted-foreground">
              {f.path}
            </p>
          )}
        </li>
      ))}
    </ul>
  )
}

/** §4.3 validation_report.hashes — path → sha256. Tamper-detection anchor. */
function HashList({ report }: { report: ValidationReport }) {
  const entries = Object.entries(report.hashes ?? {})
  if (entries.length === 0) return null
  return (
    <div className="space-y-1">
      <p className="text-xs font-medium text-muted-foreground">
        文件哈希（篡改检测锚）
      </p>
      <ul className="space-y-0.5 text-[11px] font-mono">
        {entries.slice(0, 8).map(([path, hash]) => (
          <li key={path} className="flex items-start gap-2">
            <span className="shrink-0 text-muted-foreground">{path}</span>
            <span className="truncate text-foreground/80">{hash}</span>
          </li>
        ))}
        {entries.length > 8 && (
          <li className="text-muted-foreground">… 共 {entries.length} 项</li>
        )}
      </ul>
    </div>
  )
}

/**
 * §4.3 signature presence — Mora records signature presence/shape, it does
 * NOT verify against a key store and never surfaces secret key material
 * (§1.2). Renders a calm "signed / unsigned" indicator, never a secret.
 */
function SignatureIndicator({ report }: { report: ValidationReport }) {
  const sig = report.signature
  const hasSig = sig && Object.keys(sig).length > 0
  return (
    <Badge variant={hasSig ? "secondary" : "outline"}>
      <ShieldCheck className="size-3" />
      {hasSig ? "已签名（仅记录存在性）" : "未签名"}
    </Badge>
  )
}

export function ValidationReportView({
  status,
  report,
}: {
  status: SkillPackage["validation_status"]
  report: ValidationReport
}) {
  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <FileWarning className="size-4 text-muted-foreground" />
        <span className="text-sm font-medium">静态校验报告</span>
        <ValidationStatusBadge status={status} />
        <SignatureIndicator report={report} />
      </div>
      <p className="text-xs text-muted-foreground">
        validation_status=passed 仅表示可保存/可交付，不代表可执行（Mora 不执行 Skill）。
      </p>
      <FindingsList report={report} />
      <Separator />
      <HashList report={report} />
    </div>
  )
}

export function CompatibilityReportView({
  report,
}: {
  report: CompatibilityReport
}) {
  const needs = report.runtime_needs ?? []
  const opaque = report.opaque_fields ?? []
  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <Boxes className="size-4 text-muted-foreground" />
        <span className="text-sm font-medium">兼容性报告</span>
        <DeliveryVerdictBadge status={report.delivery} />
      </div>
      {needs.length > 0 && (
        <div className="space-y-1">
          <p className="text-xs font-medium text-muted-foreground">
            运行时需求（Mora 无法满足，需运行时适配）
          </p>
          <div className="flex flex-wrap gap-1">
            {needs.map((n) => (
              <Badge key={n} variant="warning">
                {n}
              </Badge>
            ))}
          </div>
        </div>
      )}
      {opaque.length > 0 && (
        <div className="space-y-1">
          <p className="text-xs font-medium text-muted-foreground">
            未识别字段（已原样保留，无损往返）
          </p>
          <div className="flex flex-wrap gap-1">
            {opaque.map((f) => (
              <Badge key={f} variant="outline">
                {f}
              </Badge>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

/** Full skill package report panel (validation + compatibility). Used in the
 * skill version detail view. No secret values anywhere (§1.2). */
export function SkillPackageReport({ pkg }: { pkg: SkillPackage }) {
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
        <span>format: <code className="font-mono">{pkg.format_id}</code></span>
        <span>·</span>
        <span>schema: {pkg.schema_version}</span>
        <span>·</span>
        <span>content_hash: <code className="font-mono">{pkg.content_hash}</code></span>
        <span>·</span>
        <span>scanner: {pkg.scanner_version}</span>
        <span>·</span>
        <span>更新: {fmtTime(pkg.updated_at)}</span>
      </div>
      <ValidationReportView
        status={pkg.validation_status}
        report={pkg.validation_report}
      />
      <Separator />
      <CompatibilityReportView report={pkg.compatibility_report} />
    </div>
  )
}
