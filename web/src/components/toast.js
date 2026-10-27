import { reactive } from 'vue'

export const toasts = reactive([])
let seq = 0

export function toast(message, kind = 'ok') {
  const id = ++seq
  toasts.push({ id, message, kind })
  setTimeout(() => {
    const i = toasts.findIndex((t) => t.id === id)
    if (i >= 0) toasts.splice(i, 1)
  }, kind === 'error' ? 6000 : 3000)
}

export function toastError(e) {
  toast(e?.message || String(e), 'error')
}
