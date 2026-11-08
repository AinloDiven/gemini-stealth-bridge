package server

import (
	"context"
	"log"
	"strings"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

// skillTurnState keeps inner[81] and byte-0 @skill synchronized with the active
// account and page session across failover, re-bootstrap, and retries.
type skillTurnState struct {
	boundAccountID string
	bound          bool
	target         string
}

func (st *skillTurnState) invalidate() { st.bound = false }

func (st *skillTurnState) prepare(ctx context.Context, s *Server, scope AuthScope, account *AccountState, opts *gemini.StreamOptions, prompt string) string {
	if opts != nil && opts.Temporary {
		opts.SkillRuntime = ""
		return prompt
	}
	acctID := ""
	if account != nil {
		acctID = account.ID
	}
	if !st.bound || st.boundAccountID != acctID {
		st.target = s.skillTargetFor(scope, account)
		opts.SkillRuntime = ""
		if st.target != "" && account != nil {
			if binder, ok := account.Client.(skillRuntimeBinder); ok {
				if handle, err := binder.BindSkillRuntime(ctx, st.target); err == nil {
					opts.SkillRuntime = handle
				} else {
					log.Printf("cloud Skill %q could not be bound for account %s: %v; turn runs without slot 81", st.target, account.ID, err)
				}
			}
		}
		st.boundAccountID = acctID
		st.bound = true
	}
	return formatSkillPrompt(prompt, st.target)
}

func (s *Server) skillTargetFor(scope AuthScope, account *AccountState) string {
	s.mu.RLock()
	perKey := strings.TrimSpace(s.Cfg.SkillPerKey[scope.Token])
	if perKey == "" && scope.Pool {
		perKey = strings.TrimSpace(s.Cfg.SkillPerKey[s.Cfg.APIKey])
		if perKey == "" && s.Cfg.APIKey == "public" {
			perKey = strings.TrimSpace(s.Cfg.SkillPerKey["pool"])
		}
	}
	global := strings.TrimSpace(s.Cfg.Skill)
	s.mu.RUnlock()
	if perKey != "" {
		return perKey
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

func formatSkillPrompt(text, skill string) string {
	prefix := strings.TrimSpace(skill)
	if prefix == "" {
		return text
	}
	if !strings.HasPrefix(prefix, "@") {
		prefix = "@" + prefix
	}
	stripped := strings.TrimSpace(text)
	if stripped == "" {
		return prefix
	}
	if stripped == prefix || strings.HasPrefix(stripped, prefix+" ") || strings.HasPrefix(stripped, prefix+"\n") {
		return text
	}
	if strings.HasPrefix(stripped, prefix) {
		rest := strings.TrimLeft(stripped[len(prefix):], " \t\r\n")
		if rest == "" {
			return prefix
		}
		return prefix + " " + rest
	}
	return prefix + " " + text
}
