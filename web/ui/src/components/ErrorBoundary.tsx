import { Component, type ReactNode } from 'react'

/** Keeps one broken panel from blanking the whole page. */
export class ErrorBoundary extends Component<{ children: ReactNode; label: string }, { error: Error | null }> {
  state = { error: null as Error | null }
  static getDerivedStateFromError(error: Error) {
    return { error }
  }
  render() {
    if (!this.state.error) return this.props.children
    return (
      <div className="rounded-md border border-destructive/40 bg-destructive/5 p-4 text-sm">
        <p className="font-medium">The {this.props.label} could not be shown.</p>
        <p className="mt-1 font-mono text-xs text-muted-foreground">{this.state.error.message}</p>
      </div>
    )
  }
}
