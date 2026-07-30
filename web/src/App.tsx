import { WikiLayout } from "@/components/wiki/WikiLayout"
import { TooltipProvider } from "@/components/ui/tooltip"

function App() {
  return (
    <TooltipProvider>
      <WikiLayout />
    </TooltipProvider>
  )
}

export default App
