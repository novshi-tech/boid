package orchestrator

// The daemon's self-recorded card state changes: creation, a content edit and
// an identity binding each mutate a card outside the action log's reach (the
// task row, the identity index), so each writes one of these alongside the
// mutation, in the same transaction.
const (
	// ActionTypeCardCreated marks a card's own creation.
	ActionTypeCardCreated = "created"
	// ActionTypeCardEdited marks a card's own title/description having been
	// replaced. One edit is one record — the unit is the operation, not the
	// field — and the payload names the fields, never their bodies.
	ActionTypeCardEdited = "edited"
	// ActionTypeIdentityLinked marks one identity having been bound to a card.
	ActionTypeIdentityLinked = "identity_linked"
	// ActionTypeIdentityUnlinked marks one identity having been released from
	// a card.
	ActionTypeIdentityUnlinked = "identity_unlinked"
)

// CardEditedField is ActionTypeCardEdited's closed field vocabulary.
const (
	CardEditedFieldTitle       = "title"
	CardEditedFieldDescription = "description"
)

// CardEditedPayload is ActionTypeCardEdited's payload.
type CardEditedPayload struct {
	Fields []string `json:"fields"`
}

// IdentityLinkedPayload is ActionTypeIdentityLinked's and
// ActionTypeIdentityUnlinked's payload.
type IdentityLinkedPayload struct {
	Identity string `json:"identity"`
}

// cardStateSelfRecords is the set of the four types above — the card actions
// the daemon writes about a card's own shape rather than about work on it.
var cardStateSelfRecords = map[string]bool{
	ActionTypeCardCreated:      true,
	ActionTypeCardEdited:       true,
	ActionTypeIdentityLinked:   true,
	ActionTypeIdentityUnlinked: true,
}

// IsCardStateSelfRecord reports whether actionType is one of them.
func IsCardStateSelfRecord(actionType string) bool {
	return cardStateSelfRecords[actionType]
}
