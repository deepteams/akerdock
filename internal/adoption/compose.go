package adoption

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// RewriteComposeExternalVolumes redeclares every named volume of the file as
// `external` under its CURRENT Docker name (`<project>_<key>`, or the
// explicit `name:` when one was set). This is what makes the §20.7
// acceptance hold: the normalizing redeployment mounts the SAME volumes —
// letting the engine prefix them with the new stack uuid would silently
// remount empty ones.
//
// The rewrite is node surgery on the volumes section only: the rest of the
// document — comments included — is preserved.
func RewriteComposeExternalVolumes(content, project string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return "", fmt.Errorf("yaml: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("yaml: not a compose mapping document")
	}
	root := doc.Content[0]

	var volumes *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "volumes" {
			volumes = root.Content[i+1]
		}
	}
	if volumes == nil || volumes.Kind != yaml.MappingNode {
		return content, nil // no named volumes: nothing to pin
	}

	for i := 0; i+1 < len(volumes.Content); i += 2 {
		key := volumes.Content[i].Value
		val := volumes.Content[i+1]
		name := project + "_" + key
		if val.Kind == yaml.MappingNode {
			if explicit := mappingValue(val, "name"); explicit != "" {
				name = explicit
			}
			if isTrue(mappingValue(val, "external")) {
				continue // already external: it is already pinned
			}
		}
		replacement := &yaml.Node{Kind: yaml.MappingNode}
		replacement.Content = []*yaml.Node{
			scalar("external"),
			{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
			scalar("name"), scalar(name),
		}
		volumes.Content[i+1] = replacement
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("yaml: %w", err)
	}
	return string(out), nil
}

func mappingValue(n *yaml.Node, key string) string {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1].Value
		}
	}
	return ""
}

func isTrue(v string) bool {
	return strings.EqualFold(v, "true")
}

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}
