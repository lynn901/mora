import { useEffect, useRef, useCallback } from "react"
import { useEditor, EditorContent } from "@tiptap/react"
import StarterKit from "@tiptap/starter-kit"
import CodeBlockLowlight from "@tiptap/extension-code-block-lowlight"
import TaskList from "@tiptap/extension-task-list"
import TaskItem from "@tiptap/extension-task-item"
import Underline from "@tiptap/extension-underline"
import Placeholder from "@tiptap/extension-placeholder"
import Image from "@tiptap/extension-image"
import Link from "@tiptap/extension-link"
import TextAlign from "@tiptap/extension-text-align"
import { common, createLowlight } from "lowlight"
import { Markdown } from "tiptap-markdown"
import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Separator } from "@/components/ui/separator"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle"
import {
  Bold, Italic, Underline as UnderlineIcon, Strikethrough, Code, CodeSquare,
  Heading1, Heading2, Heading3, List, ListOrdered, ListChecks,
  Quote, Link as LinkIcon, Image as ImageIcon, AlignLeft, AlignCenter, AlignRight, Undo, Redo
} from "lucide-react"
import { useWikiStore } from "@/stores/wiki"

const lowlight = createLowlight(common)

const MarkdownExt = Markdown.configure({
  html: false,
  transformCopiedText: true,
  transformPastedText: true,
})

export function BlockEditor() {
  const { currentDocument, editorMode, setEditorMode, updateDocument, isDirty, saveDocument } = useWikiStore()
  const autoSaveRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const editor = useEditor({
    extensions: [
      StarterKit.configure({
        codeBlock: false,
        heading: { levels: [1, 2, 3] },
      }),
      CodeBlockLowlight.configure({ lowlight }),
      TaskList,
      TaskItem.configure({ nested: true }),
      Underline,
      Image,
      Link.configure({ openOnClick: false }),
      TextAlign.configure({ types: ["heading", "paragraph"] }),
      Placeholder.configure({ placeholder: "Start writing..." }),
      MarkdownExt,
    ],
    content: currentDocument?.content || "",
    onUpdate: ({ editor }) => {
      const content = editor.getHTML()
      updateDocument({ content })
      if (autoSaveRef.current) clearTimeout(autoSaveRef.current)
      autoSaveRef.current = setTimeout(() => { saveDocument() }, 5000)
    },
    editorProps: {
      attributes: { class: "prose prose-sm dark:prose-invert max-w-none focus:outline-none min-h-[300px] px-8 py-6" },
    },
  })

  useEffect(() => {
    if (editor && currentDocument) {
      const currentContent = editor.getHTML()
      if (currentContent !== currentDocument.content) {
        editor.commands.setContent(currentDocument.content)
      }
    }
  }, [currentDocument?.id])

  useEffect(() => {
    return () => {
      if (autoSaveRef.current) clearTimeout(autoSaveRef.current)
    }
  }, [])

  if (!editor) return null

  return (
    <div className="flex flex-col h-full">
      <div className="flex items-center gap-1 px-4 py-2 border-b overflow-x-auto">
        <ToggleGroup type="multiple" className="gap-0">
          <ToggleGroupItem value="bold" aria-label="Bold" onClick={() => editor.chain().focus().toggleBold().run()} data-state={editor.isActive("bold") ? "on" : "off"}>
            <Bold className="size-4" />
          </ToggleGroupItem>
          <ToggleGroupItem value="italic" aria-label="Italic" onClick={() => editor.chain().focus().toggleItalic().run()} data-state={editor.isActive("italic") ? "on" : "off"}>
            <Italic className="size-4" />
          </ToggleGroupItem>
          <ToggleGroupItem value="underline" aria-label="Underline" onClick={() => editor.chain().focus().toggleUnderline().run()} data-state={editor.isActive("underline") ? "on" : "off"}>
            <UnderlineIcon className="size-4" />
          </ToggleGroupItem>
          <ToggleGroupItem value="strike" aria-label="Strikethrough" onClick={() => editor.chain().focus().toggleStrike().run()} data-state={editor.isActive("strike") ? "on" : "off"}>
            <Strikethrough className="size-4" />
          </ToggleGroupItem>
          <ToggleGroupItem value="code" aria-label="Inline code" onClick={() => editor.chain().focus().toggleCode().run()} data-state={editor.isActive("code") ? "on" : "off"}>
            <Code className="size-4" />
          </ToggleGroupItem>
        </ToggleGroup>

        <Separator orientation="vertical" className="mx-1 h-6" />

        <ToggleGroup type="single" className="gap-0">
          <ToggleGroupItem value="h1" aria-label="Heading 1" onClick={() => editor.chain().focus().toggleHeading({ level: 1 }).run()} data-state={editor.isActive("heading", { level: 1 }) ? "on" : "off"}>
            <Heading1 className="size-4" />
          </ToggleGroupItem>
          <ToggleGroupItem value="h2" aria-label="Heading 2" onClick={() => editor.chain().focus().toggleHeading({ level: 2 }).run()} data-state={editor.isActive("heading", { level: 2 }) ? "on" : "off"}>
            <Heading2 className="size-4" />
          </ToggleGroupItem>
          <ToggleGroupItem value="h3" aria-label="Heading 3" onClick={() => editor.chain().focus().toggleHeading({ level: 3 }).run()} data-state={editor.isActive("heading", { level: 3 }) ? "on" : "off"}>
            <Heading3 className="size-4" />
          </ToggleGroupItem>
        </ToggleGroup>

        <Separator orientation="vertical" className="mx-1 h-6" />

        <ToggleGroup type="multiple" className="gap-0">
          <ToggleGroupItem value="bullet" aria-label="Bullet list" onClick={() => editor.chain().focus().toggleBulletList().run()} data-state={editor.isActive("bulletList") ? "on" : "off"}>
            <List className="size-4" />
          </ToggleGroupItem>
          <ToggleGroupItem value="ordered" aria-label="Ordered list" onClick={() => editor.chain().focus().toggleOrderedList().run()} data-state={editor.isActive("orderedList") ? "on" : "off"}>
            <ListOrdered className="size-4" />
          </ToggleGroupItem>
          <ToggleGroupItem value="task" aria-label="Task list" onClick={() => editor.chain().focus().toggleTaskList().run()} data-state={editor.isActive("taskList") ? "on" : "off"}>
            <ListChecks className="size-4" />
          </ToggleGroupItem>
          <ToggleGroupItem value="codeblock" aria-label="Code block" onClick={() => editor.chain().focus().toggleCodeBlock().run()} data-state={editor.isActive("codeBlock") ? "on" : "off"}>
            <CodeSquare className="size-4" />
          </ToggleGroupItem>
          <ToggleGroupItem value="quote" aria-label="Blockquote" onClick={() => editor.chain().focus().toggleBlockquote().run()} data-state={editor.isActive("blockquote") ? "on" : "off"}>
            <Quote className="size-4" />
          </ToggleGroupItem>
        </ToggleGroup>

        <Separator orientation="vertical" className="mx-1 h-6" />

        <ToggleGroup type="single" className="gap-0">
          <ToggleGroupItem value="left" aria-label="Align left" onClick={() => editor.chain().focus().setTextAlign("left").run()} data-state={editor.isActive({ textAlign: "left" }) ? "on" : "off"}>
            <AlignLeft className="size-4" />
          </ToggleGroupItem>
          <ToggleGroupItem value="center" aria-label="Align center" onClick={() => editor.chain().focus().setTextAlign("center").run()} data-state={editor.isActive({ textAlign: "center" }) ? "on" : "off"}>
            <AlignCenter className="size-4" />
          </ToggleGroupItem>
          <ToggleGroupItem value="right" aria-label="Align right" onClick={() => editor.chain().focus().setTextAlign("right").run()} data-state={editor.isActive({ textAlign: "right" }) ? "on" : "off"}>
            <AlignRight className="size-4" />
          </ToggleGroupItem>
        </ToggleGroup>

        <Separator orientation="vertical" className="mx-1 h-6" />

        <Button variant="ghost" size="icon" className="size-8" onClick={() => editor.chain().focus().undo().run()} disabled={!editor.can().undo()} aria-label="Undo">
          <Undo className="size-4" />
        </Button>
        <Button variant="ghost" size="icon" className="size-8" onClick={() => editor.chain().focus().redo().run()} disabled={!editor.can().redo()} aria-label="Redo">
          <Redo className="size-4" />
        </Button>

        <div className="ml-auto flex items-center gap-2">
          <ToggleGroup type="single" value={editorMode} onValueChange={(v: string) => v && setEditorMode(v as "wysiwyg" | "markdown")}>
            <ToggleGroupItem value="wysiwyg">WYSIWYG</ToggleGroupItem>
            <ToggleGroupItem value="markdown">Markdown</ToggleGroupItem>
          </ToggleGroup>
          {isDirty && <span className="text-xs text-muted-foreground">Saving...</span>}
        </div>
      </div>

      <div className="flex-1 overflow-auto">
        {editorMode === "wysiwyg" ? (
          <EditorContent editor={editor} />
        ) : (
          <textarea
            className="w-full h-full min-h-[300px] p-6 font-mono text-sm bg-transparent resize-none focus:outline-none"
            value={currentDocument?.content || ""}
            onChange={(e) => updateDocument({ content: e.target.value })}
            placeholder="Write in Markdown..."
            aria-label="Markdown editor"
          />
        )}
      </div>
    </div>
  )
}
