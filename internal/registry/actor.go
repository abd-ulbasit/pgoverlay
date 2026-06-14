package registry

import "context"

// Actor identifies WHO performed a branch mutation: the API token name and the
// role it resolved to. It is carried on a request context from the API
// middleware down to the registry, where TransitionBranch (and the other
// transition-journal writers) stamp it onto the transitions row's actor column.
// An empty Actor formats as the system actor (see String).
type Actor struct {
	Name string // token name, or a sentinel like "root"/"env-token"
	Role string // admin | operator | viewer
}

// SystemActor is the recorded actor for transitions with no request actor on
// their context — internal reconcile/GC operations and other daemon-initiated
// state changes.
const SystemActor = "system:reconcile"

// String renders the actor as "name (role)", or the bare name when no role is
// known, or SystemActor when the actor is empty. This is the exact text stored
// in the transitions.actor column and shown in the audit history.
func (a Actor) String() string {
	if a.Name == "" {
		return SystemActor
	}
	if a.Role == "" {
		return a.Name
	}
	return a.Name + " (" + a.Role + ")"
}

type actorCtxKey struct{}

// WithActor returns a copy of ctx carrying the given actor. The API middleware
// calls this after resolving a bearer token so downstream engine/registry
// operations can record who initiated the mutation.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, a)
}

// ActorFromContext returns the actor stashed by WithActor. When none is present
// (a daemon-initiated operation: reconcile, GC, TTL expiry) it returns the
// zero Actor, which records as SystemActor.
func ActorFromContext(ctx context.Context) Actor {
	if ctx != nil {
		if a, ok := ctx.Value(actorCtxKey{}).(Actor); ok {
			return a
		}
	}
	return Actor{}
}

// actorString resolves the actor on ctx to the string stored in the
// transitions.actor column (SystemActor when no actor is present).
func actorString(ctx context.Context) string {
	return ActorFromContext(ctx).String()
}
