package dbstate

// GitEnrollmentState is git_enrollments.state. A workspace's read capability is usable only while
// the enrollment is published and its credential is too.
type GitEnrollmentState string

// Git enrollment states.
const (
	GitEnrollmentPlanned   GitEnrollmentState = "planned"
	GitEnrollmentPublished GitEnrollmentState = "published"
	GitEnrollmentRevoked   GitEnrollmentState = "revoked"
)

// GitGrantState is git_push_grants.state, with the same lifecycle as an enrollment.
type GitGrantState string

// Git push grant states.
const (
	GitGrantPlanned   GitGrantState = "planned"
	GitGrantPublished GitGrantState = "published"
	GitGrantRevoked   GitGrantState = "revoked"
)

// GitPublicationState is git_publications.state. Dispatching and unknown are unresolved: the
// remote may have taken the update, so the gate reads it before acting again.
type GitPublicationState string

// Git publication states.
const (
	PublicationPending     GitPublicationState = "pending"
	PublicationDispatching GitPublicationState = "dispatching"
	PublicationConfirmed   GitPublicationState = "confirmed"
	PublicationRejected    GitPublicationState = "rejected"
	PublicationUnknown     GitPublicationState = "unknown"
)
