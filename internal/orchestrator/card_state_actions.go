package orchestrator

// The daemon's self-recorded card state changes: creation, description
// rewrite and identity link each mutate a card outside the action log's
// reach (the task row and the identity index), so each writes one of these
// alongside the mutation, in the same transaction.
//
// Like the ActionTypeCommand* family these are written through CreateAction
// directly and never through Apply; the card machine registers them only so
// the names are known.
const (
	// ActionTypeCardCreated marks a card's own creation.
	ActionTypeCardCreated = "created"
	// ActionTypeDescriptionSet marks a card's description having been
	// replaced. The payload carries no body — the task row holds it.
	ActionTypeDescriptionSet = "description_set"
	// ActionTypeIdentityLinked marks one identity having been bound to a
	// card. The payload carries the identity.
	ActionTypeIdentityLinked = "identity_linked"
)

// IdentityLinkedPayload is ActionTypeIdentityLinked's payload.
type IdentityLinkedPayload struct {
	Identity string `json:"identity"`
}
