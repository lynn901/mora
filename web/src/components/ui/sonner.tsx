/* eslint-disable react-refresh/only-export-components */
import { Toaster as SonnerToaster, toast } from "sonner"

/**
 * Mora toast host. Mounted once at the app root. Use the `toast` export for
 * imperative feedback (save/delete/rollback results). Styled with the design
 * tokens so toasts match cards in both light and dark themes.
 */
export function Toaster() {
  return (
    <SonnerToaster
      position="bottom-right"
      richColors
      closeButton
      toastOptions={{
        classNames: {
          toast:
            "group toast group-[.toaster]:bg-card group-[.toaster]:text-card-foreground group-[.toaster]:border-border group-[.toaster]:shadow-lg",
          description: "group-[.toast]:text-muted-foreground",
        },
      }}
    />
  )
}

export { toast }

