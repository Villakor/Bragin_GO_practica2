package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

type errOut struct {
	file string
	line int // 0 => без номера строки
	msg  string
}

type reporter struct {
	file string
	errs []errOut
}

func (r *reporter) add(line int, msg string) {
	r.errs = append(r.errs, errOut{file: r.file, line: line, msg: msg})
}

func (r *reporter) addRequiredNoLine(field string) {
	r.errs = append(r.errs, errOut{file: r.file, line: 0, msg: fmt.Sprintf("%s is required", field)})
}

func (r *reporter) addRequiredAt(line int, field string) {
	r.errs = append(r.errs, errOut{file: r.file, line: line, msg: fmt.Sprintf("%s is required", field)})
}

func (r *reporter) hasErrors() bool { return len(r.errs) > 0 }

func (r *reporter) flushToStdout() {
	for _, e := range r.errs {
		if e.line > 0 {
			fmt.Printf("%s:%d %s\n", e.file, e.line, e.msg)
		} else {
			fmt.Printf("%s: %s\n", e.file, e.msg)
		}
	}
}

var (
	reSnake       = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	reMem         = regexp.MustCompile(`^\d+(Gi|Mi|Ki)$`)
	reImage       = regexp.MustCompile(`^registry\.bigbrother\.io/.+:.+$`)
	validOS       = map[string]struct{}{"linux": {}, "windows": {}}
	validProtocol = map[string]struct{}{"TCP": {}, "UDP": {}}
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: yamlvalid <path-to-yaml>")
		os.Exit(2)
	}
	file := os.Args[1]
	content, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read file: %v\n", err)
		os.Exit(1)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		fmt.Fprintf(os.Stderr, "cannot unmarshal file content: %v\n", err)
		os.Exit(1)
	}
	if len(root.Content) == 0 {
		fmt.Fprintln(os.Stderr, "cannot unmarshal file content: empty document")
		os.Exit(1)
	}

	abs := file
	if !filepath.IsAbs(file) {
		if a, err := filepath.Abs(file); err == nil {
			abs = a
		}
	}

	rep := &reporter{file: filepath.Base(abs)}

	for _, doc := range root.Content {
		if doc.Kind != yaml.MappingNode {
			rep.add(doc.Line, "root must be object")
			continue
		}
		validatePod(doc, rep)
	}

	if rep.hasErrors() {
		rep.flushToStdout() // важно: stdout
		os.Exit(1)
	}
	os.Exit(0)
}

// ===== Валидация верхнего уровня =====

func validatePod(doc *yaml.Node, rep *reporter) {
	// apiVersion (required string == v1)
	if n, ok := getField(doc, "apiVersion"); !ok {
		rep.addRequiredNoLine("apiVersion")
	} else {
		if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
			rep.add(n.Line, "apiVersion must be string")
		} else if n.Value != "v1" {
			rep.add(n.Line, fmt.Sprintf("apiVersion has unsupported value '%s'", n.Value))
		}
	}

	// kind (required string == Pod)
	if n, ok := getField(doc, "kind"); !ok {
		rep.addRequiredNoLine("kind")
	} else {
		if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
			rep.add(n.Line, "kind must be string")
		} else if n.Value != "Pod" {
			rep.add(n.Line, fmt.Sprintf("kind has unsupported value '%s'", n.Value))
		}
	}

	// metadata (required ObjectMeta)
	if n, ok := getField(doc, "metadata"); !ok {
		rep.addRequiredNoLine("metadata")
	} else {
		if n.Kind != yaml.MappingNode {
			rep.add(n.Line, "metadata must be object")
		} else {
			validateObjectMeta(n, rep)
		}
	}

	// spec (required PodSpec)
	if n, ok := getField(doc, "spec"); !ok {
		rep.addRequiredNoLine("spec")
	} else {
		if n.Kind != yaml.MappingNode {
			rep.add(n.Line, "spec must be object")
		} else {
			validatePodSpec(n, rep)
		}
	}
}

// ===== ObjectMeta =====

func validateObjectMeta(n *yaml.Node, rep *reporter) {
	if name, ok := getField(n, "name"); !ok {
		rep.addRequiredNoLine("metadata.name")
	} else {
		if name.Kind != yaml.ScalarNode || name.Tag != "!!str" {
			rep.add(name.Line, "name must be string")
		} else if strings.TrimSpace(name.Value) == "" {
			// Пустое значение — как «обязательное поле» с линией (для автотестов)
			rep.addRequiredAt(name.Line, "name")
		}
	}

	if ns, ok := getField(n, "namespace"); ok {
		if ns.Kind != yaml.ScalarNode || ns.Tag != "!!str" {
			rep.add(ns.Line, "namespace must be string")
		}
	}

	if labels, ok := getField(n, "labels"); ok {
		if labels.Kind != yaml.MappingNode {
			rep.add(labels.Line, "labels must be object")
		} else {
			for i := 0; i < len(labels.Content); i += 2 {
				k := labels.Content[i]
				v := labels.Content[i+1]
				if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
					rep.add(k.Line, "labels key must be string")
				}
				if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
					rep.add(v.Line, "labels value must be string")
				}
			}
		}
	}
}

// ===== PodSpec =====

func validatePodSpec(n *yaml.Node, rep *reporter) {
	// os (optional): строка linux/windows или объект { name: ... }
	if osNode, ok := getField(n, "os"); ok {
		switch osNode.Kind {
		case yaml.ScalarNode:
			if osNode.Tag != "!!str" {
				rep.add(osNode.Line, "os must be string")
			} else if _, ok := validOS[osNode.Value]; !ok {
				rep.add(osNode.Line, fmt.Sprintf("os has unsupported value '%s'", osNode.Value))
			}
		case yaml.MappingNode:
			if nameNode, ok := getField(osNode, "name"); !ok {
				rep.addRequiredNoLine("spec.os.name")
			} else if nameNode.Kind != yaml.ScalarNode || nameNode.Tag != "!!str" {
				rep.add(nameNode.Line, "os.name must be string")
			} else if _, ok := validOS[nameNode.Value]; !ok {
				rep.add(nameNode.Line, fmt.Sprintf("os has unsupported value '%s'", nameNode.Value))
			}
		default:
			rep.add(osNode.Line, "os must be string or object")
		}
	}

	// containers (required []Container)
	containers, ok := getField(n, "containers")
	if !ok {
		rep.addRequiredNoLine("spec.containers")
		return
	}
	if containers.Kind != yaml.SequenceNode {
		rep.add(containers.Line, "containers must be array")
		return
	}
	if len(containers.Content) == 0 {
		rep.add(containers.Line, "containers value out of range")
	}

	seenNames := map[string]struct{}{}
	for _, item := range containers.Content {
		if item.Kind != yaml.MappingNode {
			rep.add(item.Line, "container must be object")
			continue
		}
		validateContainer(item, rep, seenNames)
	}
}

// ===== Container =====

func validateContainer(item *yaml.Node, rep *reporter, seen map[string]struct{}) {
	// name (required, snake_case, unique)
	var cname string
	if nameNode, ok := getField(item, "name"); !ok {
		rep.addRequiredNoLine("containers.name")
	} else {
		if nameNode.Kind != yaml.ScalarNode || nameNode.Tag != "!!str" {
			rep.add(nameNode.Line, "name must be string")
		} else if strings.TrimSpace(nameNode.Value) == "" {
			// пустая строка → "name is required" с линией
			rep.addRequiredAt(nameNode.Line, "name")
		} else if !reSnake.MatchString(nameNode.Value) {
			rep.add(nameNode.Line, fmt.Sprintf("name has invalid format '%s'", nameNode.Value))
		} else {
			cname = nameNode.Value
			if _, exists := seen[cname]; exists {
				rep.add(nameNode.Line, "name has invalid format 'duplicate'")
			}
			seen[cname] = struct{}{}
		}
	}

	// image (required, registry.bigbrother.io, с тегом)
	if img, ok := getField(item, "image"); !ok {
		rep.addRequiredNoLine("containers.image")
	} else {
		if img.Kind != yaml.ScalarNode || img.Tag != "!!str" {
			rep.add(img.Line, "image must be string")
		} else if !reImage.MatchString(img.Value) {
			rep.add(img.Line, fmt.Sprintf("image has invalid format '%s'", img.Value))
		}
	}

	// ports (optional) — массив объектов ContainerPort
	if ports, ok := getField(item, "ports"); ok {
		if ports.Kind != yaml.SequenceNode {
			rep.add(ports.Line, "ports must be array")
		} else {
			for _, pn := range ports.Content {
				if pn.Kind != yaml.MappingNode {
					rep.add(pn.Line, "ports item must be object")
					continue
				}
				validateContainerPort(pn, rep)
			}
		}
	}

	// readinessProbe / livenessProbe (optional)
	if rp, ok := getField(item, "readinessProbe"); ok {
		validateProbe(rp, rep, "readinessProbe")
	}
	if lp, ok := getField(item, "livenessProbe"); ok {
		validateProbe(lp, rep, "livenessProbe")
	}

	// resources (required)
	if res, ok := getField(item, "resources"); !ok {
		rep.addRequiredNoLine("containers.resources")
	} else {
		validateResources(res, rep)
	}
}

// ===== ContainerPort =====

func validateContainerPort(n *yaml.Node, rep *reporter) {
	if cp, ok := getField(n, "containerPort"); !ok {
		rep.addRequiredNoLine("ports.containerPort")
	} else {
		ival, line, err := asInt(cp)
		if err != nil {
			rep.add(line, "containerPort must be int")
		} else if ival <= 0 || ival >= 65536 {
			rep.add(line, "containerPort value out of range")
		}
	}
	if pr, ok := getField(n, "protocol"); ok {
		if pr.Kind != yaml.ScalarNode || pr.Tag != "!!str" {
			rep.add(pr.Line, "protocol must be string")
		} else if _, ok := validProtocol[pr.Value]; !ok {
			rep.add(pr.Line, fmt.Sprintf("protocol has unsupported value '%s'", pr.Value))
		}
	}
}

// ===== Probe =====

func validateProbe(n *yaml.Node, rep *reporter, prefix string) {
	if n.Kind != yaml.MappingNode {
		rep.add(n.Line, fmt.Sprintf("%s must be object", prefix))
		return
	}
	hg, ok := getField(n, "httpGet")
	if !ok {
		rep.addRequiredNoLine(prefix + ".httpGet")
		return
	}
	if hg.Kind != yaml.MappingNode {
		rep.add(hg.Line, "httpGet must be object")
		return
	}
	if path, ok := getField(hg, "path"); !ok {
		rep.addRequiredNoLine(prefix + ".httpGet.path")
	} else {
		if path.Kind != yaml.ScalarNode || path.Tag != "!!str" {
			rep.add(path.Line, "path must be string")
		} else if !strings.HasPrefix(path.Value, "/") {
			rep.add(path.Line, fmt.Sprintf("path has invalid format '%s'", path.Value))
		}
	}
	if port, ok := getField(hg, "port"); !ok {
		rep.addRequiredNoLine(prefix + ".httpGet.port")
	} else {
		ival, line, err := asInt(port)
		if err != nil {
			rep.add(line, "port must be int")
		} else if ival <= 0 || ival >= 65536 {
			rep.add(line, "port value out of range")
		}
	}
}

// ===== Resources =====

func validateResources(n *yaml.Node, rep *reporter) {
	if n.Kind != yaml.MappingNode {
		rep.add(n.Line, "resources must be object")
		return
	}
	if lim, ok := getField(n, "limits"); ok {
		validateResKV(lim, rep, "resources.limits")
	}
	if req, ok := getField(n, "requests"); ok {
		validateResKV(req, rep, "resources.requests")
	}
}

func validateResKV(n *yaml.Node, rep *reporter, prefix string) {
	if n.Kind != yaml.MappingNode {
		rep.add(n.Line, prefix+" must be object")
		return
	}
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
			rep.add(k.Line, prefix+" key must be string")
			continue
		}
		switch k.Value {
		case "cpu":
			ival, line, err := asInt(v)
			if err != nil {
				rep.add(line, "cpu must be int")
			} else if ival < 0 {
				rep.add(line, "cpu value out of range")
			}
		case "memory":
			if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
				rep.add(v.Line, "memory must be string")
			} else if !reMem.MatchString(v.Value) {
				rep.add(v.Line, fmt.Sprintf("%s.memory has invalid format '%s'", prefix, v.Value))
			}
		default:
			// игнорируем прочие ключи
		}
	}
}

// ===== YAML helpers =====

func getField(obj *yaml.Node, key string) (*yaml.Node, bool) {
	if obj.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i < len(obj.Content); i += 2 {
		k := obj.Content[i]
		v := obj.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.Value == key {
			return v, true
		}
	}
	return nil, false
}

func asInt(n *yaml.Node) (int, int, error) {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!int" {
			v, err := strconv.Atoi(n.Value)
			return v, n.Line, err
		}
		if n.Tag == "!!str" {
			v, err := strconv.Atoi(strings.TrimSpace(n.Value))
			if err != nil {
				return 0, n.Line, errors.New("not int")
			}
			return v, n.Line, nil
		}
		return 0, n.Line, errors.New("not int")
	default:
		return 0, n.Line, errors.New("not int")
	}
}
