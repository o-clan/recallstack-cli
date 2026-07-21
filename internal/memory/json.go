package memory

import "encoding/json"

func jsonMarshalIndent(value any) ([]byte, error) { return json.MarshalIndent(value, "", "  ") }
