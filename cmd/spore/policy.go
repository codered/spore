package main

import (
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
)

// cmdPolicyCheck answers "what would spore do with this call?" without
// running anything — the way to test a ruleset after editing it.
func cmdPolicyCheck(cfg *config.Config, profile, toolName, argsJSON string) error {
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
	res := engine.Evaluate(policy.Profile(profile), policy.Call{Tool: toolName, Args: json.RawMessage(argsJSON)})
	fmt.Printf("%s\t%s\t%s\n", res.Decision, toolName, res.Rule)
	if res.Decision == policy.DecisionAsk {
		fmt.Printf("  \"always this pattern\" would write: %s\n", policy.PatternFor(
			policy.Call{Tool: toolName, Args: json.RawMessage(argsJSON)}))
	}
	return nil
}
