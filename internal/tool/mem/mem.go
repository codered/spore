// Package mem exposes spore's memory layers to the model: recall_search over
// the index, and memory over the fact files. Both live here because they are
// two views of one subsystem, and because the fact directory is the only path
// either of them may touch.
package mem

import (
	"encoding/json"
	"fmt"
)

func decode(args json.RawMessage, dst any) error {
	if len(args) == 0 {
		return fmt.Errorf("no arguments supplied")
	}
	if err := json.Unmarshal(args, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
