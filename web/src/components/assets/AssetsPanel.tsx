// Phase 1-6 — assets panel rendered in the MoraLayout main area when the
// "assets" side-tab is active. Toggles between the unified asset list and
// the asset detail view (with version history) using local state so the
// navigation stays within the single-page shell.
import { useState } from "react"
import { AssetListPage } from "./AssetListPage"
import { AssetDetailPage } from "./AssetDetailPage"

export function AssetsPanel() {
  const [openAssetId, setOpenAssetId] = useState<string | null>(null)

  if (openAssetId) {
    return (
      <AssetDetailPage
        assetId={openAssetId}
        onBack={() => setOpenAssetId(null)}
      />
    )
  }
  return <AssetListPage onOpenAsset={setOpenAssetId} />
}
