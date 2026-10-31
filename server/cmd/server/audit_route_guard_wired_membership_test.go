package main

func init() {
	for route, kind := range map[string]string{
		"POST /api/workspaces/{id}/members":                      "invitation.create",
		"DELETE /api/workspaces/{id}/invitations/{invitationId}": "invitation.revoke",
		"POST /api/invitations/{id}/accept":                      "invitation.accept",
		"POST /api/invitations/{id}/decline":                     "invitation.decline",
		"PATCH /api/workspaces/{id}/members/{memberId}/":         "member.update",
		"DELETE /api/workspaces/{id}/members/{memberId}/":        "member.remove",
		"POST /api/workspaces/{id}/leave":                        "member.leave",
		"POST /api/workspaces/{id}/share-links":                  "share_link.create",
		"PUT /api/workspaces/{id}/delegated-writes":              "workspace.delegated_writes.update",
		"PATCH /api/workspaces/{id}/governance/config":           "governance.config.update",
		"PATCH /api/workspaces/{id}/export-privacy":              "workspace.export_privacy.update",
		"PUT /api/workspaces/{id}/jev/credential":                "jev.credential.set",
		"DELETE /api/workspaces/{id}/jev/credential":             "jev.credential.delete",
	} {
		auditedRoutes[route] = kind
		delete(auditPendingL5b, route)
	}
}
