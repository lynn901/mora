import { ObservableV2 } from "lib0/observable"
import * as encoding from "lib0/encoding"
import * as decoding from "lib0/decoding"
import {
  writeSyncStep1,
  writeSyncStep2,
  readSyncStep1,
  readSyncStep2,
  readSyncMessage,
} from "y-protocols/sync"
import {
  Awareness,
  encodeAwarenessUpdate,
  applyAwarenessUpdate,
  removeAwarenessStates,
} from "y-protocols/awareness"
import * as Y from "yjs"

const MESSAGE_SYNC = 0
const MESSAGE_AWARENESS = 1

const BACKOFF_BASE = 1000
const BACKOFF_MAX = 30000
const AWARENESS_INTERVAL = 15000
const SYNC_TIMEOUT = 5000

export type CollabProviderStatus = "connecting" | "connected" | "disconnected" | "degraded" | "denied" | "local-only"

export interface CollabProviderEvents {
  status: (status: CollabProviderStatus) => void
  synced: () => void
}

export interface CollabProviderOptions {
  serverUrl: string
  documentId: string
  token: string
  userId: string
  userName: string
  userColor: string
}

function toBase64(bytes: Uint8Array): string {
  let binary = ""
  for (let i = 0; i < bytes.byteLength; i++) {
    binary += String.fromCharCode(bytes[i])
  }
  return btoa(binary)
}

function fromBase64(base64: string): Uint8Array {
  const binary = atob(base64)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i)
  }
  return bytes
}

interface BackendMessage {
  type: string
  from?: string
  payload?: unknown
}

interface PresenceEntry {
  user_id: string
  name: string
  color: string
  read_only: boolean
}

export class MoraCollabProvider extends ObservableV2<CollabProviderEvents> {
  public doc: Y.Doc
  public awareness: Awareness
  public status: CollabProviderStatus = "disconnected"

  private ws: WebSocket | null = null
  private opts: CollabProviderOptions
  private reconnectAttempts = 0
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private awarenessTimer: ReturnType<typeof setInterval> | null = null
  private syncTimer: ReturnType<typeof setTimeout> | null = null
  private _synced = false
  private shouldConnect = false

  constructor(opts: CollabProviderOptions) {
    super()
    this.opts = opts
    this.doc = new Y.Doc()
    this.awareness = new Awareness(this.doc)

    this.doc.on("update", (update: Uint8Array, origin: unknown) => {
      if (origin !== this) {
        this.sendSyncMessage(update)
      }
    })

    this.awareness.on("update", ({ added, updated, removed }: { added: number[]; updated: number[]; removed: number[] }) => {
      const changedClients = added.concat(updated).concat(removed)
      const update = encodeAwarenessUpdate(this.awareness, changedClients)
      this.sendAwarenessMessage(update)
    })

    this.awareness.setLocalState({
      user: {
        id: opts.userId,
        name: opts.userName,
        color: opts.userColor,
      },
    })
  }

  connect() {
    if (this.shouldConnect) return
    this.shouldConnect = true
    this.createConnection()
  }

  disconnect() {
    this.shouldConnect = false
    this.clearTimers()
    this.clearSyncTimeout()
    if (this.ws) {
      const ws = this.ws
      this.ws = null
      try {
        ws.send(JSON.stringify({
          type: "cursor",
          payload: { user_id: this.opts.userId, cursor: null },
        }))
      } catch {
        void 0
      }
      ws.close()
    }
    this.setStatus("disconnected")
    removeAwarenessStates(this.awareness, [this.doc.clientID], this)
  }

  destroy() {
    this.disconnect()
    this.doc.destroy()
    this.awareness.destroy()
    super.destroy()
  }

  private createConnection() {
    if (this.ws) return

    const url = new URL(this.opts.serverUrl)
    url.searchParams.set("token", this.opts.token)

    const ws = new WebSocket(url.toString())
    ws.binaryType = "arraybuffer"
    this.ws = ws
    this.setStatus("connecting")

    ws.addEventListener("message", (event) => {
      this.handleMessage(event.data)
    })

    ws.addEventListener("open", () => {
      this.reconnectAttempts = 0
      this.setStatus("connected")
      this.initSync()
      this.startAwarenessHeartbeat()
      this.startSyncTimeout()
    })

    ws.addEventListener("close", () => {
      this.ws = null
      this._synced = false
      this.setStatus("disconnected")
      removeAwarenessStates(this.awareness, [this.doc.clientID], this)
      if (this.shouldConnect) {
        this.scheduleReconnect()
      }
    })

    ws.addEventListener("error", () => {})
  }

  private handleMessage(data: string | ArrayBuffer) {
    let msg: BackendMessage
    if (typeof data === "string") {
      try {
        msg = JSON.parse(data) as BackendMessage
      } catch {
        return
      }
    } else {
      return
    }

    switch (msg.type) {
      case "presence":
        this.handlePresenceRoster(msg.payload as PresenceEntry[])
        break
      case "join":
        break
      case "leave":
        break
      case "cursor":
        break
      case "update":
        this.handleRemoteUpdate(msg.payload as string)
        break
      case "awareness":
        this.handleRemoteAwareness(msg.payload as string)
        break
      case "sync-step1":
        this.handleSyncStep1(msg.payload as string)
        break
      case "sync-step2":
        this.handleSyncStep2(msg.payload as string)
        break
      case "degraded":
        this.setStatus("degraded")
        break
      case "denied":
        this.setStatus("denied")
        this.shouldConnect = false
        if (this.ws) this.ws.close()
        break
    }
  }

  private initSync() {
    const encoder = encoding.createEncoder()
    writeSyncStep1(encoder, this.doc)
    const data = toBase64(encoding.toUint8Array(encoder))
    this.sendJson({ type: "sync-step1", payload: data })
  }

  private sendSyncMessage(update: Uint8Array) {
    const encoder = encoding.createEncoder()
    encoding.writeVarUint(encoder, MESSAGE_SYNC)
    encoding.writeVarUint8Array(encoder, update)
    const data = toBase64(encoding.toUint8Array(encoder))
    this.sendJson({ type: "update", payload: data })
  }

  private sendAwarenessMessage(update: Uint8Array) {
    const encoder = encoding.createEncoder()
    encoding.writeVarUint(encoder, MESSAGE_AWARENESS)
    encoding.writeVarUint8Array(encoder, update)
    const data = toBase64(encoding.toUint8Array(encoder))
    this.sendJson({ type: "awareness", payload: data })
  }

  private handleRemoteUpdate(payloadBase64: string) {
    if (!payloadBase64) return
    const bytes = fromBase64(payloadBase64)
    const decoder = decoding.createDecoder(bytes)
    const messageType = decoding.readVarUint(decoder)
    if (messageType === MESSAGE_SYNC) {
      const encoder = encoding.createEncoder()
      readSyncMessage(decoder, encoder, this.doc, this)
      const response = encoding.toUint8Array(encoder)
      if (response.byteLength > 0) {
        this.sendJson({ type: "update", payload: toBase64(response) })
      }
      if (!this._synced) {
        this._synced = true
        this.clearSyncTimeout()
        this.emit("synced", [])
      }
    }
  }

  private handleRemoteAwareness(payloadBase64: string) {
    if (!payloadBase64) return
    const bytes = fromBase64(payloadBase64)
    const decoder = decoding.createDecoder(bytes)
    const messageType = decoding.readVarUint(decoder)
    if (messageType === MESSAGE_AWARENESS) {
      const updateData = decoding.readVarUint8Array(decoder)
      applyAwarenessUpdate(this.awareness, updateData, this)
    }
  }

  private handleSyncStep1(payloadBase64: string) {
    if (!payloadBase64) return
    const bytes = fromBase64(payloadBase64)
    const decoder = decoding.createDecoder(bytes)
    const messageType = decoding.readVarUint(decoder)
    if (messageType === 0) {
      const encoder = encoding.createEncoder()
      readSyncStep1(decoder, encoder, this.doc)
      const step2Data = encoding.toUint8Array(encoder)
      if (step2Data.byteLength > 0) {
        this.sendJson({ type: "sync-step2", payload: toBase64(step2Data) })
      }
      const syncEncoder = encoding.createEncoder()
      writeSyncStep2(syncEncoder, this.doc)
      const step2Response = encoding.toUint8Array(syncEncoder)
      if (step2Response.byteLength > 0) {
        this.sendJson({ type: "sync-step2", payload: toBase64(step2Response) })
      }
      const initEncoder = encoding.createEncoder()
      writeSyncStep1(initEncoder, this.doc)
      const step1Response = encoding.toUint8Array(initEncoder)
      if (step1Response.byteLength > 0) {
        this.sendJson({ type: "sync-step1", payload: toBase64(step1Response) })
      }
    }
  }

  private handleSyncStep2(payloadBase64: string) {
    if (!payloadBase64) return
    const bytes = fromBase64(payloadBase64)
    const decoder = decoding.createDecoder(bytes)
    const messageType = decoding.readVarUint(decoder)
    if (messageType === 1) {
      const updateData = decoding.readVarUint8Array(decoder)
      readSyncStep2(decoding.createDecoder(updateData), this.doc, this)
      if (!this._synced) {
        this._synced = true
        this.clearSyncTimeout()
        this.emit("synced", [])
      }
    }
  }

  private handlePresenceRoster(roster: PresenceEntry[]) {
    void roster
  }

  private sendJson(msg: Record<string, unknown>) {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(msg))
    }
  }

  private setStatus(status: CollabProviderStatus) {
    if (this.status === status) return
    this.status = status
    this.emit("status", [status])
  }

  private scheduleReconnect() {
    if (this.reconnectTimer) return
    const delay = Math.min(BACKOFF_BASE * 2 ** this.reconnectAttempts, BACKOFF_MAX)
    this.reconnectAttempts++
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null
      if (this.shouldConnect && !this.ws) {
        this.createConnection()
      }
    }, delay)
  }

  private startAwarenessHeartbeat() {
    this.clearAwarenessTimer()
    this.awarenessTimer = setInterval(() => {
      const localState = this.awareness.getLocalState()
      if (localState) {
        const update = encodeAwarenessUpdate(this.awareness, [this.doc.clientID])
        this.sendAwarenessMessage(update)
      }
    }, AWARENESS_INTERVAL)
  }

  private clearAwarenessTimer() {
    if (this.awarenessTimer) {
      clearInterval(this.awarenessTimer)
      this.awarenessTimer = null
    }
  }

  private clearTimers() {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    this.clearAwarenessTimer()
  }

  private startSyncTimeout() {
    this.clearSyncTimeout()
    this.syncTimer = setTimeout(() => {
      if (!this._synced) {
        this.setStatus("local-only")
      }
    }, SYNC_TIMEOUT)
  }

  private clearSyncTimeout() {
    if (this.syncTimer) {
      clearTimeout(this.syncTimer)
      this.syncTimer = null
    }
  }
}
