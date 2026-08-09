import { lazy, Suspense } from "react"
import { BrowserRouter, Routes, Route, Navigate } from "react-router-dom"
import { LoginPage } from "@/components/auth/LoginPage"
import { TooltipProvider } from "@/components/ui/tooltip"
import { ThemeProvider } from "@/components/theme-provider"
import { useAuthStore } from "@/stores/auth"

const MoraLayout = lazy(() =>
  import("@/components/mora/MoraLayout").then((module) => ({
    default: module.MoraLayout,
  }))
)

const LlmGatewayPanel = lazy(() =>
  import("@/components/parse/LlmGatewayPanel").then((module) => ({
    default: module.LlmGatewayPanel,
  }))
)

function ProtectedRoute({ children }: { children: React.ReactNode }) {
  const { isAuthenticated } = useAuthStore()
  if (!isAuthenticated) return <Navigate to="/login" replace />
  return <>{children}</>
}

function App() {
  return (
    <ThemeProvider defaultTheme="system" storageKey="mora-theme">
      <TooltipProvider>
        <BrowserRouter>
          <Suspense
            fallback={
              <div className="flex h-screen items-center justify-center text-sm text-muted-foreground">
                Loading...
              </div>
            }
          >
            <Routes>
              <Route path="/login" element={<LoginPage />} />
              <Route
                path="/admin/llm-gateway"
                element={
                  <ProtectedRoute>
                    <MoraLayout>
                      <LlmGatewayPanel />
                    </MoraLayout>
                  </ProtectedRoute>
                }
              />
              <Route
                path="/*"
                element={
                  <ProtectedRoute>
                    <MoraLayout />
                  </ProtectedRoute>
                }
              />
            </Routes>
          </Suspense>
        </BrowserRouter>
      </TooltipProvider>
    </ThemeProvider>
  )
}

export default App
