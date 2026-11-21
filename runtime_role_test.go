package profiles_test

// Match the deployed runtime privileges on the tables used by closure and
// retention. Migration owns the security-definer functions and restricts PUBLIC.
func runtimeRoleSQL(role string) string {
	return `CREATE ROLE ` + role + ` LOGIN;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA profiles TO ` + role + `;
GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA profiles TO ` + role + `;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA profiles TO ` + role + `;
REVOKE UPDATE,DELETE ON profiles.requests,profiles.events,profiles.closure_requests,profiles.closure_events FROM ` + role + `;
REVOKE UPDATE,DELETE,TRUNCATE ON profiles.closure_completions FROM ` + role + `;
REVOKE DELETE ON profiles.closures FROM ` + role + `;
GRANT EXECUTE ON FUNCTION profiles.expire_profile_request(text,text,text,timestamptz),
profiles.expire_closure_request(text,text,text,timestamptz),profiles.erase_closed_account_requests(text,text),
profiles.cleanup_expired(timestamptz,integer),profiles.ack_closure_completion(text) TO ` + role + `;`
}
