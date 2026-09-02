package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

type ChangeKind string

const (
	ChangeAdd    ChangeKind = "add"
	ChangeModify ChangeKind = "modify"
	ChangeRemove ChangeKind = "remove"
)

type Change struct {
	Kind   ChangeKind `json:"kind"`
	Path   string     `json:"path"`
	Before any        `json:"before,omitempty"`
	After  any        `json:"after,omitempty"`
}

func Diff(before, after *Manifest) ([]Change, error) {
	if after == nil {
		return nil, fmt.Errorf("diff manifests: target manifest is nil")
	}
	if before == nil {
		changes := []Change{{Kind: ChangeAdd, Path: "application", After: after.Name}}
		changes = append(changes, Change{Kind: ChangeAdd, Path: "deploy", After: after.Deploy})
		for _, name := range sortedKeys(after.Services) {
			changes = append(changes, Change{Kind: ChangeAdd, Path: "services." + name, After: after.Services[name]})
		}
		for _, name := range sortedKeys(after.Volumes) {
			changes = append(changes, Change{Kind: ChangeAdd, Path: "volumes." + name, After: after.Volumes[name]})
		}
		return changes, nil
	}

	left, err := asMap(before)
	if err != nil {
		return nil, err
	}
	right, err := asMap(after)
	if err != nil {
		return nil, err
	}
	changes := make([]Change, 0)
	walkDiff("", left, right, &changes)
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].Path == changes[j].Path {
			return changes[i].Kind < changes[j].Kind
		}
		return changes[i].Path < changes[j].Path
	})
	return changes, nil
}

func FormatChange(change Change) string {
	prefix := map[ChangeKind]string{
		ChangeAdd:    "+",
		ChangeModify: "~",
		ChangeRemove: "-",
	}[change.Kind]
	switch change.Kind {
	case ChangeAdd:
		return fmt.Sprintf("%s %s: %s", prefix, change.Path, compactValue(change.After))
	case ChangeRemove:
		return fmt.Sprintf("%s %s: %s", prefix, change.Path, compactValue(change.Before))
	default:
		return fmt.Sprintf("%s %s: %s -> %s", prefix, change.Path, compactValue(change.Before), compactValue(change.After))
	}
}

func asMap(value any) (map[string]any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode diff input: %w", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode diff input: %w", err)
	}
	return result, nil
}

func walkDiff(prefix string, before, after any, changes *[]Change) {
	leftMap, leftOK := before.(map[string]any)
	rightMap, rightOK := after.(map[string]any)
	if !leftOK || !rightOK {
		if !reflect.DeepEqual(before, after) {
			*changes = append(*changes, Change{Kind: ChangeModify, Path: prefix, Before: before, After: after})
		}
		return
	}

	keys := make(map[string]struct{}, len(leftMap)+len(rightMap))
	for key := range leftMap {
		keys[key] = struct{}{}
	}
	for key := range rightMap {
		keys[key] = struct{}{}
	}
	ordered := sortedKeys(keys)
	for _, key := range ordered {
		field := key
		if prefix != "" {
			field = prefix + "." + key
		}
		left, leftExists := leftMap[key]
		right, rightExists := rightMap[key]
		switch {
		case !leftExists:
			*changes = append(*changes, Change{Kind: ChangeAdd, Path: field, After: right})
		case !rightExists:
			*changes = append(*changes, Change{Kind: ChangeRemove, Path: field, Before: left})
		default:
			walkDiff(field, left, right, changes)
		}
	}
}

func compactValue(value any) string {
	if value == nil {
		return "null"
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	const limit = 180
	result := []rune(string(data))
	if len(result) > limit {
		return strings.TrimSpace(string(result[:limit])) + "…"
	}
	return string(result)
}
