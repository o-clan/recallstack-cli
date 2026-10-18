package cli

import (
	"fmt"
	"strings"
)

type optionSpec struct {
	values map[string]struct{}
	bools  map[string]struct{}
	lists  map[string]struct{}
}

type parsedOptions struct {
	positionals []string
	lists       map[string][]string
	values      map[string]string
	bools       map[string]bool
	present     map[string]bool
}

func newOptionSpec(valueNames []string, boolNames []string) optionSpec {
	spec := optionSpec{
		values: make(map[string]struct{}, len(valueNames)),
		bools:  make(map[string]struct{}, len(boolNames)+1),
	}
	for _, name := range valueNames {
		spec.values[name] = struct{}{}
	}
	for _, name := range boolNames {
		spec.bools[name] = struct{}{}
	}
	spec.bools["help"] = struct{}{}
	spec.lists = map[string]struct{}{}
	return spec
}

// withLists marks value options that may repeat (--instead-of a --instead-of b).
func (spec optionSpec) withLists(names ...string) optionSpec {
	for _, name := range names {
		spec.values[name] = struct{}{}
		spec.lists[name] = struct{}{}
	}
	return spec
}

func parseOptions(args []string, spec optionSpec) (parsedOptions, error) {
	parsed := parsedOptions{
		values:  map[string]string{},
		lists:   map[string][]string{},
		bools:   map[string]bool{},
		present: map[string]bool{},
	}

	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			parsed.positionals = append(parsed.positionals, args[index+1:]...)
			break
		}
		if arg == "-h" {
			arg = "--help"
		}
		if !strings.HasPrefix(arg, "--") {
			parsed.positionals = append(parsed.positionals, arg)
			continue
		}

		nameValue := strings.TrimPrefix(arg, "--")
		name, value, hasValue := strings.Cut(nameValue, "=")
		if _, ok := spec.bools[name]; ok {
			if hasValue {
				return parsed, fmt.Errorf("option --%s does not take a value", name)
			}
			parsed.bools[name] = true
			parsed.present[name] = true
			continue
		}
		if _, ok := spec.values[name]; !ok {
			return parsed, fmt.Errorf("unknown option --%s", name)
		}
		if !hasValue {
			index++
			if index >= len(args) {
				return parsed, fmt.Errorf("option --%s requires a value", name)
			}
			value = args[index]
		}
		if value == "" {
			return parsed, fmt.Errorf("option --%s requires a value", name)
		}
		if _, ok := spec.lists[name]; ok {
			parsed.lists[name] = append(parsed.lists[name], value)
		}
		parsed.values[name] = value
		parsed.present[name] = true
	}

	return parsed, nil
}
