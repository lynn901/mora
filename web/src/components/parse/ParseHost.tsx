// ParseHost — mounts the parse-related dialogs/drawer once at the layout level
// so any trigger (tree upload button, document badge, monitoring table) can
// open them via the parse store without prop-drilling. Render inside MoraLayout.
import { UploadParseDialog } from "./UploadParseDialog"
import { ParseProgressDrawer } from "./ParseProgressDrawer"
import { BatchReparseDialog } from "./BatchReparseDialog"
import { useParseStore } from "@/stores/parse"

export function ParseHost() {
  const uploadOpen = useParseStore((s) => s.uploadOpen)
  const setUploadOpen = useParseStore((s) => s.setUploadOpen)
  return (
    <>
      <UploadParseDialog open={uploadOpen} onOpenChange={setUploadOpen} />
      <ParseProgressDrawer />
      <BatchReparseDialog />
    </>
  )
}
