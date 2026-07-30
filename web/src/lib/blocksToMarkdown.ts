interface Block {
  type: string
  attrs?: Record<string, unknown>
  content?: Block[]
  text?: string
  marks?: { type: string; attrs?: Record<string, unknown> }[]
}

function renderMarks(text: string, marks?: Block["marks"]): string {
  if (!marks || marks.length === 0) return text
  let result = text
  for (const mark of marks) {
    switch (mark.type) {
      case "bold":
      case "strong":
        result = `**${result}**`
        break
      case "italic":
      case "em":
        result = `*${result}*`
        break
      case "strike":
      case "strikethrough":
        result = `~~${result}~~`
        break
      case "code":
        result = `\`${result}\``
        break
      case "link":
        result = `[${result}](${mark.attrs?.href || ""})`
        break
    }
  }
  return result
}

function renderInline(content?: Block[]): string {
  if (!content) return ""
  return content
    .map((node) => {
      if (node.type === "text") return renderMarks(node.text || "", node.marks)
      if (node.type === "hardBreak") return "\n"
      return node.text || ""
    })
    .join("")
}

function blockToMarkdown(block: Block, depth = 0): string {
  const indent = "  ".repeat(depth)

  switch (block.type) {
    case "heading": {
      const level = (block.attrs?.level as number) || 1
      return `${indent}${"#".repeat(level)} ${renderInline(block.content)}`
    }
    case "paragraph":
      return `${indent}${renderInline(block.content)}`
    case "codeBlock": {
      const lang = (block.attrs?.language as string) || ""
      const code = block.content?.map((n) => n.text || "").join("\n") || ""
      return `${indent}\`\`\`${lang}\n${indent}${code}\n${indent}\`\`\``
    }
    case "blockquote":
      return (block.content || []).map((c) => `> ${blockToMarkdown(c)}`).join("\n")
    case "bulletList":
      return (block.content || []).map((c) => blockToMarkdown(c, depth)).join("\n")
    case "orderedList":
      return (block.content || [])
        .map((c, i) => `${indent}${i + 1}. ${renderInline(c.content)}`)
        .join("\n")
    case "listItem":
      return `${indent}- ${renderInline(block.content)}`
    case "horizontalRule":
    case "divider":
      return `${indent}---`
    case "image":
      return `${indent}![${block.attrs?.alt || ""}](${block.attrs?.src || ""})`
    case "taskList":
      return (block.content || []).map((c) => blockToMarkdown(c, depth)).join("\n")
    case "taskItem": {
      const checked = block.attrs?.checked ? "x" : " "
      return `${indent}- [${checked}] ${renderInline(block.content)}`
    }
    default:
      if (block.content) return block.content.map((c) => blockToMarkdown(c, depth)).join("\n")
      return block.text || ""
  }
}

export function blocksToMarkdown(blocks: Block[]): string {
  if (!blocks || blocks.length === 0) return ""
  return blocks.map((b) => blockToMarkdown(b)).join("\n\n")
}
