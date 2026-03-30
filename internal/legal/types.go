package legal

// Document types stored in legal_documents / legal_acceptances.
// legal_pending_resume.kind values are defined in bots (e.g. driver_relive = re-share live location after legal interrupt; driver_accept = resume order accept).
const (
	DocDriverTerms   = "driver_terms"
	DocUserTerms     = "user_terms"
	// DocPrivacyPolicy is the legacy shared privacy policy (kept for history/backward compatibility only).
	DocPrivacyPolicy = "privacy_policy"
	// Split privacy policies: users should never see driver-specific document collection text.
	DocPrivacyPolicyUser   = "privacy_policy_user"
	DocPrivacyPolicyDriver = "privacy_policy_driver"
	ErrCodeRequired  = "LEGAL_ACCEPTANCE_REQUIRED"
	RiderDocTypes    = 2
	DriverDocTypes   = 2 // driver_terms + privacy_policy_driver (riders accept user_terms + privacy_policy_user)
)

// SQLDriverDispatchLegalOK is appended to driver dispatch queries: driver_terms and driver privacy policy at active versions.
// Expects outer alias `d` for drivers with d.user_id.
const SQLDriverDispatchLegalOK = `2 = (
  SELECT COUNT(*) FROM legal_acceptances la
  INNER JOIN legal_documents ld ON ld.document_type = la.document_type AND ld.version = la.version AND ld.is_active = 1
  WHERE la.user_id = d.user_id
  AND la.document_type IN ('driver_terms','privacy_policy_driver')
)`
