import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

/** Exchanges the factory token for an HttpOnly session cookie. */
export function SignIn({ open }: { open: boolean }) {
  const [token, setToken] = useState('')
  const [error, setError] = useState('')
  return (
    <Dialog open={open}>
      <DialogContent showCloseButton={false} onEscapeKeyDown={(e) => e.preventDefault()} onPointerDownOutside={(e) => e.preventDefault()}>
        <DialogHeader>
          <DialogTitle>Sign in to this factory</DialogTitle>
          <DialogDescription>It runs code on its host, so it asks for the token it was started with (FLIESSBAND_TOKEN).</DialogDescription>
        </DialogHeader>
        <form
          className="space-y-3"
          onSubmit={async (e) => {
            e.preventDefault()
            const r = await fetch('/api/session', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ token }) })
            if (r.ok) location.reload()
            else setError(r.status === 401 ? 'That token is not right.' : `Sign-in failed: ${r.statusText}`)
          }}
        >
          <div className="space-y-1.5">
            <Label htmlFor="token">Token</Label>
            <Input id="token" type="password" autoComplete="current-password" autoFocus required value={token} onChange={(e) => setToken(e.target.value)} />
          </div>
          <p className="min-h-5 text-sm text-destructive" aria-live="polite">
            {error}
          </p>
          <Button type="submit" className="w-full">
            Sign in
          </Button>
        </form>
      </DialogContent>
    </Dialog>
  )
}
