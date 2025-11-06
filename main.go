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
	// line == 0 означает, что строка не печатается (случай "<field> is required")
	line int
	msg  string
}

type reporter struct {
	file string
	errs []errOut
}

func (r *reporter) add(line int, msg string) {
	r.errs = append(r.errs, errOut{file: r.file, line: line, msg: msg})
}

func (r *reporter) addRequired(field string) {
	// формат без номера строки
	r.errs = append(r.errs, errOut{file: r.file, line: 0, msg: fmt.Sprintf("%s is required", field)})
}

func (r *reporter) hasErrors() bool { return len(r.errs) > 0 }

func (r *reporter) flushToStderr() {
	for _, e := range r.errs {
		if e.line > 0 {
			fmt.Fprintf(os.Stderr, "%s:%d %s\n", r.file, e.line, e.msg)
		} else {
			// без номера строки
			fmt.Fprintf(os.Stderr, "%s: %s\n", r.file, e.msg)
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
	abs := file
	if !filepath.IsAbs(file) {
		if a, err := filepath.Abs(file); err == nil {
			abs = a
		}
	}

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
	// Ожидаем документы YAML (root.Kind == DocumentNode)
	if root.Kind == 0 && len(root.Content) == 0 {
		fmt.Fprintf(os.Stderr, "cannot unmarshal file content: empty document\n")
		os.Exit(1)
	}

	rep := &reporter{file: file}

	// Поддержим несколько документов, но валидируем каждый как Pod
	for _, doc := range root.Content {
		if doc.Kind != yaml.MappingNode {
			rep.add(doc.Line, "root must be object")
			continue
		}
		validatePod(abs, doc, rep)
	}

	if rep.hasErrors() {
		rep.flushToStderr()
		os.Exit(1)
	}
	os.Exit(0)
}

func validatePod(filename string, doc *yaml.Node, rep *reporter) {
	// 1. Верхний уровень: apiVersion(kind string=v1), kind(Pod), metadata(ObjectMeta), spec(PodSpec)
	apiVersionNode, ok := getField(doc, "apiVersion")
	if !ok {
		rep.addRequired("apiVersion")
	} else {
		if apiVersionNode.Kind != yaml.ScalarNode || apiVersionNode.Tag != "!!str" {
			rep.add(apiVersionNode.Line, "apiVersion must be string")
		} else if apiVersionNode.Value != "v1" {
			rep.add(apiVersionNode.Line, fmt.Sprintf("apiVersion has unsupported value '%s'", apiVersionNode.Value))
		}
	}

	kindNode, ok := getField(doc, "kind")
	if !ok {
		rep.addRequired("kind")
	} else {
		if kindNode.Kind != yaml.ScalarNode || kindNode.Tag != "!!str" {
			rep.add(kindNode.Line, "kind must be string")
		} else if kindNode.Value != "Pod" {
			rep.add(kindNode.Line, fmt.Sprintf("kind has unsupported value '%s'", kindNode.Value))
		}
	}

	metadataNode, ok := getField(doc, "metadata")
	if !ok {
		rep.addRequired("metadata")
	} else {
		if metadataNode.Kind != yaml.MappingNode {
			rep.add(metadataNode.Line, "metadata must be object")
		} else {
			validateObjectMeta(metadataNode, rep)
		}
	}

	specNode, ok := getField(doc, "spec")
	if !ok {
		rep.addRequired("spec")
	} else {
		if specNode.Kind != yaml.MappingNode {
			rep.add(specNode.Line, "spec must be object")
		} else {
			validatePodSpec(specNode, rep)
		}
	}
}

// ObjectMeta: name (required, string, not empty), namespace (opt, string), labels (opt, object of string:string)
func validateObjectMeta(n *yaml.Node, rep *reporter) {
	name, ok := getField(n, "name")
	if !ok {
		rep.addRequired("metadata.name")
	} else {
		if name.Kind != yaml.ScalarNode || name.Tag != "!!str" {
			rep.add(name.Line, "name must be string")
		} else if strings.TrimSpace(name.Value) == "" {
			rep.add(name.Line, "name has invalid format ''")
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
			// Ensure all values are strings
			for i := 0; i < len(labels.Content); i += 2 {
				k := labels.Content[i]
				v := labels.Content[i+1]
				if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
					rep.add(v.Line, "labels value must be string")
				}
				if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
					rep.add(k.Line, "labels key must be string")
				}
			}
		}
	}
}

// spec: os (opt PodOS), containers (required []Container)
func validatePodSpec(n *yaml.Node, rep *reporter) {
	if osNode, ok := getField(n, "os"); ok {
		// По требованиям: os (PodOS) → на самом деле это строка name (linux/windows)
		// В примере: os: linux (scalar). Поддержим также объект с полем name.
		switch osNode.Kind {
		case yaml.ScalarNode:
			if osNode.Tag != "!!str" {
				rep.add(osNode.Line, "os must be string")
			} else {
				if _, ok := validOS[osNode.Value]; !ok {
					rep.add(osNode.Line, fmt.Sprintf("os has unsupported value '%s'", osNode.Value))
				}
			}
		case yaml.MappingNode:
			// Вариант с объектом: name: linux|windows
			nameNode, ok := getField(osNode, "name")
			if !ok {
				rep.addRequired("spec.os.name")
			} else if nameNode.Kind != yaml.ScalarNode || nameNode.Tag != "!!str" {
				rep.add(nameNode.Line, "os.name must be string")
			} else if _, ok := validOS[nameNode.Value]; !ok {
				rep.add(nameNode.Line, fmt.Sprintf("os has unsupported value '%s'", nameNode.Value))
			}
		default:
			rep.add(osNode.Line, "os must be string or object")
		}
	}

	containers, ok := getField(n, "containers")
	if !ok {
		rep.addRequired("spec.containers")
		return
	}
	if containers.Kind != yaml.SequenceNode {
		rep.add(containers.Line, "containers must be array")
		return
	}
	if len(containers.Content) == 0 {
		rep.add(containers.Line, "containers value out of range") // пустой список — некорректен
	}

	// Уникальность name внутри пода
	seenNames := map[string]struct{}{}

	for _, item := range containers.Content {
		if item.Kind != yaml.MappingNode {
			rep.add(item.Line, "container must be object")
			continue
		}
		var cname string
		// name (required, snake_case, unique)
		if nameNode, ok := getField(item, "name"); !ok {
			rep.addRequired("containers.name")
		} else {
			if nameNode.Kind != yaml.ScalarNode || nameNode.Tag != "!!str" {
				rep.add(nameNode.Line, "name must be string")
			} else {
				if strings.TrimSpace(nameNode.Value) == "" || !reSnake.MatchString(nameNode.Value) {
					rep.add(nameNode.Line, fmt.Sprintf("name has invalid format '%s'", nameNode.Value))
				} else {
					cname = nameNode.Value
					if _, exists := seenNames[cname]; exists {
						rep.add(nameNode.Line, "name has invalid format 'duplicate'")
					}
					seenNames[cname] = struct{}{}
				}
			}
		}

		// image (required, registry.bigbrother.io, обязателен тег)
		if imageNode, ok := getField(item, "image"); !ok {
			rep.addRequired("containers.image")
		} else {
			if imageNode.Kind != yaml.ScalarNode || imageNode.Tag != "!!str" {
				rep.add(imageNode.Line, "image must be string")
			} else if !reImage.MatchString(imageNode.Value) {
				rep.add(imageNode.Line, fmt.Sprintf("image has invalid format '%s'", imageNode.Value))
			}
		}

		// ports (opt): array of ContainerPort
		if portsNode, ok := getField(item, "ports"); ok {
			if portsNode.Kind != yaml.SequenceNode {
				rep.add(portsNode.Line, "ports must be array")
			} else {
				for _, pn := range portsNode.Content {
					if pn.Kind != yaml.MappingNode {
						rep.add(pn.Line, "ports item must be object")
						continue
					}
					validateContainerPort(pn, rep)
				}
			}
		}

		// readinessProbe (opt): Probe
		if rp, ok := getField(item, "readinessProbe"); ok {
			validateProbe(rp, rep, "readinessProbe")
		}
		// livenessProbe (opt): Probe
		if lp, ok := getField(item, "livenessProbe"); ok {
			validateProbe(lp, rep, "livenessProbe")
		}

		// resources (required): ResourceRequirements
		if res, ok := getField(item, "resources"); !ok {
			rep.addRequired("containers.resources")
		} else {
			validateResources(res, rep)
		}
	}
}

// ContainerPort: containerPort (required int, 1..65535), protocol (opt TCP|UDP)
func validateContainerPort(n *yaml.Node, rep *reporter) {
	cp, ok := getField(n, "containerPort")
	if !ok {
		rep.addRequired("ports.containerPort")
	} else {
		ival, line, typErr := asInt(cp)
		if typErr != nil {
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

// Probe: httpGet (required)
func validateProbe(n *yaml.Node, rep *reporter, prefix string) {
	if n.Kind != yaml.MappingNode {
		rep.add(n.Line, fmt.Sprintf("%s must be object", prefix))
		return
	}
	hg, ok := getField(n, "httpGet")
	if !ok {
		rep.addRequired(prefix + ".httpGet")
		return
	}
	if hg.Kind != yaml.MappingNode {
		rep.add(hg.Line, "httpGet must be object")
		return
	}
	// path (required string, absolute)
	path, ok := getField(hg, "path")
	if !ok {
		rep.addRequired(prefix + ".httpGet.path")
	} else {
		if path.Kind != yaml.ScalarNode || path.Tag != "!!str" {
			rep.add(path.Line, "path must be string")
		} else if !strings.HasPrefix(path.Value, "/") {
			rep.add(path.Line, fmt.Sprintf("path has invalid format '%s'", path.Value))
		}
	}
	// port (required int in range)
	port, ok := getField(hg, "port")
	if !ok {
		rep.addRequired(prefix + ".httpGet.port")
	} else {
		ival, line, typErr := asInt(port)
		if typErr != nil {
			rep.add(line, "port must be int")
		} else if ival <= 0 || ival >= 65536 {
			rep.add(line, "port value out of range")
		}
	}
}

// ResourceRequirements: limits (opt), requests (opt), в них cpu(int), memory(string Gi|Mi|Ki)
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
	var cpuSeen, memSeen bool

	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
			rep.add(k.Line, prefix+" key must be string")
			continue
		}
		switch k.Value {
		case "cpu":
			cpuSeen = true
			ival, line, typErr := asInt(v)
			if typErr != nil {
				rep.add(line, "cpu must be int")
			} else if ival < 0 {
				rep.add(line, "cpu value out of range")
			}
		case "memory":
			memSeen = true
			if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
				rep.add(v.Line, "memory must be string")
			} else if !reMem.MatchString(v.Value) {
				rep.add(v.Line, fmt.Sprintf("resources.limits.memory has invalid format '%s'", v.Value))
			}
		default:
			// игнорируем прочие ключи
		}
	}

	// Поля в limits/requests необязательные, поэтому отсутствия не считаем ошибкой
	_ = cpuSeen
	_ = memSeen
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
		// YAML может разбирать число как int (!!int) — норм, но если это строка с цифрами, попробуем тоже
		if n.Tag == "!!int" {
			v, err := strconv.Atoi(n.Value)
			if err != nil {
				return 0, n.Line, err
			}
			return v, n.Line, nil
		}
		// Попробуем строкой
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
