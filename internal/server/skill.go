package server

import "strings"

// skillTargetFor resolves the fixed cloud Skill for this request:
// per-key override > account file override > global BRIDGE_SKILL.
func (s *Server) skillTargetFor(scope AuthScope, account *AccountState) string {
	s.mu.RLock()
	perKey := s.Cfg.SkillPerKey[scope.Token]
	global := strings.TrimSpace(s.Cfg.Skill)
	s.mu.RUnlock()
	if strings.TrimSpace(perKey) != "" {
		return strings.TrimSpace(perKey)
	}
	if account != nil {
		account.mu.RLock()
		accountSkill := strings.TrimSpace(account.Skill)
		account.mu.RUnlock()
		if accountSkill != "" {
			return accountSkill
		}
	}
	return global
}

func normalizeSkillCommand(skill string) string {
	cmd := strings.TrimSpace(skill)
	if cmd == "" {
		return ""
	}
	if !strings.HasPrefix(cmd, "@") {
		cmd = "@" + cmd
	}
	return cmd
}

func stripLeadingSkillPrefix(value, prefix string) string {
	if prefix == "" || !strings.HasPrefix(value, prefix) {
		return value
	}
	return strings.TrimLeft(value[len(prefix):], " \t\r\n")
}

func skillPrefixAtFront(value, prefix string) bool {
	if prefix == "" {
		return false
	}
	return strings.HasPrefix(value, prefix+" ") || strings.HasPrefix(value, prefix+"\n") || value == prefix
}

func prependSkillPrefix(value, prefix string) string {
	remainder := stripLeadingSkillPrefix(value, prefix)
	if remainder == "" {
		return prefix
	}
	return prefix + " " + remainder
}

// formatSkillPrompt places the configured cloud Skill command at byte 0,
// idempotently and without changing any other character (the browser sends the
// same text projection beside inner[81]).
func formatSkillPrompt(text, skill string) string {
	prefix := normalizeSkillCommand(skill)
	if prefix == "" {
		return text
	}
	stripped := strings.TrimSpace(text)
	if stripped == "" {
		return prefix
	}
	if skillPrefixAtFront(stripped, prefix) {
		return text
	}
	if strings.HasPrefix(stripped, prefix) {
		return prependSkillPrefix(stripped, prefix)
	}
	return prefix + " " + text
}
