import { computed, type Ref } from 'vue'
import type { ChatMessage, ToolCallBlock } from '@/store/chat/types'
import type { UIExecutionLocation, UIToolApproval } from '@/composables/api/useChat'
import { canDecideToolApproval } from '@/store/chat/decisions'

// One pending tool approval flattened out of the transcript, ready to render
// as a composer-panel item. The panel only ever shows ONE of these at a time
// (the queue's head), so this shape carries everything the card needs without
// reaching back into the message tree.
export interface PendingApprovalItem {
  // approval_id doubles as the item identity: it is what the respond call
  // keys on, and what the panel's content-swap transition keys on.
  id: string
  approval: UIToolApproval
  toolCallId: string
  toolName: string
  input: Record<string, unknown> | null
  executionLocation: UIExecutionLocation | null
  // The source block itself: the approval card renders the tool's own display
  // (title, target, syntax-highlighted preview, detail component) through
  // getToolDisplay(block) — the dock port of the in-flow approval form (#840)
  // — so it needs the block, not just the flattened fields above.
  block: ToolCallBlock
}

function asRecord(value: unknown): Record<string, unknown> | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null
  return value as Record<string, unknown>
}

// Derives the pane's pending-approval queue from the transcript — a pure
// projection, no state of its own (the chat architecture rule: the render
// layer copies the ledger, it never keeps one). Per-pane by construction:
// each pane passes its own transcript in, so dockview splits each get their
// own queue even when two panes show the same session.
//
// FIFO order: the oldest unresolved approval is what the agent is blocked on,
// so it leads the queue; answering it reveals the next one underneath.
// Only approvals this viewer can act on are queued: the server must report the
// decision still accepted (pending_alive) and this viewer allowed to give it
// (viewer_can_decide). Anything else — a "zombie" whose in-process waiter is
// gone, or another user's approval — would occupy the head slot forever, so it
// stays visible as a read-only label inline in the message flow instead.
export function usePendingApprovals(messages: Readonly<Ref<ChatMessage[]>>) {
  const items = computed<PendingApprovalItem[]>(() => {
    const out: PendingApprovalItem[] = []
    for (const message of messages.value) {
      if (message.role !== 'assistant') continue
      for (const block of message.messages) {
        if (block.type !== 'tool') continue
        const approval = block.approval
        if (!approval?.approval_id) continue
        if (!canDecideToolApproval(approval)) continue
        out.push({
          id: approval.approval_id,
          approval,
          toolCallId: block.toolCallId,
          toolName: block.toolName,
          input: asRecord(block.input),
          executionLocation: block.execution_location ?? null,
          block,
        })
      }
    }
    return out
  })

  return { items }
}
