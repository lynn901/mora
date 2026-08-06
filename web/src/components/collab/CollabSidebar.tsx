import { useEffect, useState, useRef } from "react"
import { MessageSquare, X, Send, Check, Wifi, WifiOff, ShieldAlert, Edit3 } from "lucide-react"
import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { Avatar, AvatarFallback, AvatarImage } from "@/components/ui/avatar"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Tooltip, TooltipTrigger, TooltipContent } from "@/components/ui/tooltip"
import type { CollaboratorPresence, Comment } from "@/types"
import { useCollabStore } from "@/stores/collab"
import { useMoraStore } from "@/stores/mora"

function StatusIndicator({ status }: { status: string }) {
  if (status === "connected") {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="flex items-center gap-1 text-xs text-success">
            <Wifi className="size-3" />
            实时
          </span>
        </TooltipTrigger>
        <TooltipContent>已连接 · 正在实时协作</TooltipContent>
      </Tooltip>
    )
  }
  if (status === "connecting") {
    return (
      <span className="flex items-center gap-1 text-xs text-warning">
        <Wifi className="size-3 animate-pulse" />
        连接中
      </span>
    )
  }
  if (status === "degraded") {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="flex items-center gap-1 text-xs text-warning">
            <ShieldAlert className="size-3" />
            只读
          </span>
        </TooltipTrigger>
        <TooltipContent>并发已达上限 · 当前为只读模式</TooltipContent>
      </Tooltip>
    )
  }
  if (status === "local-only") {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="flex items-center gap-1 text-xs text-info">
            <Edit3 className="size-3" />
            本地
          </span>
        </TooltipTrigger>
        <TooltipContent>本地编辑模式 · 改动将直接保存</TooltipContent>
      </Tooltip>
    )
  }
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="flex items-center gap-1 text-xs text-muted-foreground">
          <WifiOff className="size-3" />
          Offline
        </span>
      </TooltipTrigger>
      <TooltipContent>Disconnected - changes won't sync</TooltipContent>
    </Tooltip>
  )
}

function PresenceAvatars({ presences }: { presences: CollaboratorPresence[] }) {
  return (
    <div className="flex -space-x-2">
      {presences.map((p) => (
        <Tooltip key={p.userId}>
          <TooltipTrigger asChild>
            <div className="relative">
              <Avatar className="size-7 border-2 border-background">
                <AvatarImage src={p.userAvatar} alt={p.userName} />
                <AvatarFallback style={{ backgroundColor: p.color, color: "white" }}>
                  {p.userName.split(" ").map((n) => n[0]).join("")}
                </AvatarFallback>
              </Avatar>
              <span className="absolute -bottom-0.5 -right-0.5 size-2.5 rounded-full bg-success border-2 border-background" />
            </div>
          </TooltipTrigger>
          <TooltipContent>{p.userName} - editing</TooltipContent>
        </Tooltip>
      ))}
    </div>
  )
}

function CommentItem({ comment, onResolve }: { comment: Comment; onResolve: (id: string) => void }) {
  const users = useMoraStore((s) => s.users)
  const author = users.find((u) => u.id === comment.createdBy)

  return (
    <div className={cn("p-3 rounded-lg border", comment.resolved && "opacity-60")}>
      <div className="flex items-start gap-2">
        <Avatar className="size-6 shrink-0">
          <AvatarFallback>{author?.name?.split(" ").map((n) => n[0]).join("") || "?"}</AvatarFallback>
        </Avatar>
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2">
            <span className="text-xs font-medium">{author?.name || "未知"}</span>
            <span className="text-[10px] text-muted-foreground">{new Date(comment.createdAt).toLocaleDateString()}</span>
            {comment.resolved && <Badge variant="secondary" className="text-[10px] px-1 py-0">已解决</Badge>}
          </div>
          <p className="text-sm mt-1">{comment.content}</p>
          {comment.mentions.length > 0 && (
            <div className="flex gap-1 mt-1">
              {comment.mentions.map((m) => (
                <span key={m} className="text-xs text-primary">@{users.find((u) => u.id === m)?.name || m}</span>
              ))}
            </div>
          )}
        </div>
        {!comment.resolved && (
          <Button variant="ghost" size="icon" className="size-6 shrink-0" onClick={() => onResolve(comment.id)}>
            <Check className="size-3.5" />
          </Button>
        )}
      </div>
      {comment.replies && comment.replies.length > 0 && (
        <div className="ml-8 mt-2 space-y-2">
          {comment.replies.map((r) => <CommentItem key={r.id} comment={r} onResolve={onResolve} />)}
        </div>
      )}
    </div>
  )
}

export function CollabSidebar() {
  const { currentDocument } = useMoraStore()
  const { presences, comments, showComments, status, toggleComments, loadComments, addComment, resolveComment } = useCollabStore()
  const [newComment, setNewComment] = useState("")
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (currentDocument) {
      loadComments(currentDocument.id)
    }
  }, [currentDocument?.id])

  const handleAddComment = async () => {
    if (!newComment.trim() || !currentDocument) return
    await addComment({
      documentId: currentDocument.id,
      content: newComment,
      createdBy: "u1",
      resolved: false,
      mentions: [],
    })
    setNewComment("")
  }

  if (!showComments) {
    return (
      <div className="flex flex-col items-center gap-2 p-2">
        <PresenceAvatars presences={presences} />
        <StatusIndicator status={status} />
        <Button variant="ghost" size="icon" className="size-8" onClick={toggleComments}>
          <MessageSquare className="size-4" />
        </Button>
      </div>
    )
  }

  return (
    <div className="flex flex-col h-full border-l">
      <div className="flex items-center justify-between p-3 border-b">
        <div className="flex items-center gap-2">
          <MessageSquare className="size-4" />
          <span className="font-medium text-sm">评论</span>
          <Badge variant="secondary" className="text-[10px]">{comments.filter((c) => !c.resolved).length}</Badge>
        </div>
        <div className="flex items-center gap-2">
          <PresenceAvatars presences={presences} />
          <StatusIndicator status={status} />
          <Button variant="ghost" size="icon" className="size-7" onClick={toggleComments}>
            <X className="size-3.5" />
          </Button>
        </div>
      </div>
      <ScrollArea className="flex-1">
        <div className="p-3 space-y-3">
          {comments.length === 0 ? (
            <div className="text-center text-sm text-muted-foreground py-8">No comments yet</div>
          ) : (
            comments.map((c) => <CommentItem key={c.id} comment={c} onResolve={resolveComment} />)
          )}
        </div>
      </ScrollArea>
      <div className="p-3 border-t">
        <div className="flex gap-2">
          <Input
            ref={inputRef}
            value={newComment}
            onChange={(e) => setNewComment(e.target.value)}
            placeholder="Add a comment... Use @ to mention"
            className="h-8 text-sm"
            onKeyDown={(e) => e.key === "Enter" && handleAddComment()}
            aria-label="Add comment"
          />
          <Button size="icon" className="size-8" onClick={handleAddComment} disabled={!newComment.trim()}>
            <Send className="size-3.5" />
          </Button>
        </div>
      </div>
    </div>
  )
}
