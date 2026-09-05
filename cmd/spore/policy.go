package main

import (
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
)

// cmdPolicyCheck answers "what would spore do with this call?" without
// running anything — the way to test a ruleset after editing it.
func cmdPolicyCheck(cfg *config.Config, profile, workspace, toolName, argsJSON string) error {
	if argsJSON == "" {
		argsJSON = "{}"
	}
	if !json.Valid([]byte(argsJSON)) {
		return fmt.Errorf("arguments are not valid JSON: %s", argsJSON)
	}
	engine, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		return err
	}
	// A session's workspace decides what "path outside workspace" means, so the
	// check takes one. Without it the ceiling is used, which is the answer for
	// "would this be allowed anywhere at all".
	res := engine.Evaluate(policy.Session{ID: "policy-check", Profile: policy.Profile(profile), Workspace: workspace}, policy.Call{Tool: toolName, Args: json.RawMessage(argsJSON)})
	fmt.Printf("%s\t%s\t%s\t%s\n", res.Decision, toolName, res.Rule, workspace)
	if res.Decision == policy.DecisionAsk {
		pattern, ok := policy.PatternFor(policy.Call{Tool: toolName, Args: json.RawMessage(argsJSON)})
		if ok {
			fmt.Printf("  \"always this pattern\" would write: %s\n", pattern)
		} else {
			fmt.Printf("  \"always this pattern\" is not offered (no pattern to generalise from)\n")
		}
	}
	return nil
}
