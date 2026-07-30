import { useEffect, useState } from "react"
import { Shield, Plus, Trash2, ChevronRight } from "lucide-react"
import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter, DialogDescription } from "@/components/ui/dialog"
import { ScrollArea } from "@/components/ui/scroll-area"
import type { Permission, User } from "@/types"
import { apiGetPermissions, apiSetPermission, apiDeletePermission, apiGetUsers } from "@/api"
import { useWikiStore } from "@/stores/wiki"

export function RBACPanel() {
  const { currentWorkspace } = useWikiStore()
  const [permissions, setPermissions] = useState<Permission[]>([])
  const [users, setUsers] = useState<User[]>([])
  const [showAdd, setShowAdd] = useState(false)
  const [newUserId, setNewUserId] = useState("")
  const [newRole, setNewRole] = useState<"read" | "write" | "admin">("read")
  const [scope, setScope] = useState<"workspace" | "directory" | "page">("workspace")

  useEffect(() => {
    if (currentWorkspace) {
      apiGetPermissions(currentWorkspace.id).then(setPermissions)
    }
    apiGetUsers().then(setUsers)
  }, [currentWorkspace])

  const handleAdd = async () => {
    if (!newUserId || !currentWorkspace) return
    const perm = await apiSetPermission({
      workspaceId: currentWorkspace.id,
      userId: newUserId,
      role: newRole,
      scope,
      inherited: false,
    })
    setPermissions([...permissions, perm])
    setShowAdd(false)
    setNewUserId("")
  }

  const handleDelete = async (id: string) => {
    await apiDeletePermission(id)
    setPermissions(permissions.filter((p) => p.id !== id))
  }

  const roleVariant = (role: string) => {
    switch (role) {
      case "admin": return "destructive" as const
      case "write": return "default" as const
      default: return "secondary" as const
    }
  }

  return (
    <div className="flex flex-col h-full">
      <div className="flex items-center justify-between p-3 border-b">
        <div className="flex items-center gap-2">
          <Shield className="size-4" />
          <span className="font-medium text-sm">Permissions</span>
        </div>
        <Dialog open={showAdd} onOpenChange={setShowAdd}>
          <DialogTrigger asChild>
            <Button size="sm" className="h-7 text-xs"><Plus className="size-3" /> Add</Button>
          </DialogTrigger>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Add Permission</DialogTitle>
              <DialogDescription>Grant access to a user or group.</DialogDescription>
            </DialogHeader>
            <div className="flex flex-col gap-3">
              <div>
                <label className="text-xs font-medium text-muted-foreground">User</label>
                <Select value={newUserId} onValueChange={setNewUserId}>
                  <SelectTrigger className="h-8 text-sm"><SelectValue placeholder="Select user" /></SelectTrigger>
                  <SelectContent>
                    {users.map((u) => <SelectItem key={u.id} value={u.id}>{u.name}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
              <div>
                <label className="text-xs font-medium text-muted-foreground">Role</label>
                <Select value={newRole} onValueChange={(v) => setNewRole(v as "read" | "write" | "admin")}>
                  <SelectTrigger className="h-8 text-sm"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="read">Read</SelectItem>
                    <SelectItem value="write">Write</SelectItem>
                    <SelectItem value="admin">Admin</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div>
                <label className="text-xs font-medium text-muted-foreground">Scope</label>
                <Select value={scope} onValueChange={(v) => setScope(v as "workspace" | "directory" | "page")}>
                  <SelectTrigger className="h-8 text-sm"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="workspace">Workspace</SelectItem>
                    <SelectItem value="directory">Directory</SelectItem>
                    <SelectItem value="page">Page</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
            <DialogFooter>
              <Button variant="outline" size="sm" onClick={() => setShowAdd(false)}>Cancel</Button>
              <Button size="sm" onClick={handleAdd} disabled={!newUserId}>Add</Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      </div>
      <ScrollArea className="flex-1">
        <div className="p-2">
          {permissions.length === 0 ? (
            <div className="text-center text-sm text-muted-foreground py-8">No permissions configured</div>
          ) : (
            permissions.map((p) => {
              const user = users.find((u) => u.id === p.userId)
              return (
                <div key={p.id} className="flex items-center gap-2 p-2 rounded-lg hover:bg-accent/50 group">
                  <div className="flex-1 min-w-0">
                    <div className="text-sm font-medium truncate">{user?.name || p.userId}</div>
                    <div className="flex items-center gap-1 mt-0.5">
                      <Badge variant={roleVariant(p.role)} className="text-[10px] px-1.5 py-0">{p.role}</Badge>
                      <span className="text-[10px] text-muted-foreground flex items-center gap-0.5">
                        {p.scope} {p.inherited && <><ChevronRight className="size-2.5" /> inherited</>}
                      </span>
                    </div>
                  </div>
                  <Button variant="ghost" size="icon" className="size-7 opacity-0 group-hover:opacity-100 transition-opacity" onClick={() => handleDelete(p.id)}>
                    <Trash2 className="size-3.5 text-destructive" />
                  </Button>
                </div>
              )
            })
          )}
        </div>
      </ScrollArea>
    </div>
  )
}
