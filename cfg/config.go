package cfg

import (
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bitly/go-simplejson"
	"github.com/ghodss/yaml"
	"github.com/ozonru/file.d/logger"
	"github.com/pkg/errors"
)

type Config struct {
	Vault     VaultConfig
	Pipelines map[string]*PipelineConfig
}

type (
	Duration      string
	ListMap       string
	Expression    string
	FieldSelector string
	Regexp        string
	Base8         string
)

type PipelineConfig struct {
	Raw *simplejson.Json
}

type VaultConfig struct {
	Token     string
	Address   string
	ShouldUse bool
}

func NewConfig() *Config {
	return &Config{
		Vault: VaultConfig{
			Token:     "",
			Address:   "",
			ShouldUse: false,
		},
		Pipelines: make(map[string]*PipelineConfig, 20),
	}
}

func NewConfigFromFile(path string) *Config {
	logger.Infof("reading config %q", path)
	yamlContents, err := ioutil.ReadFile(path)
	if err != nil {
		logger.Fatalf("can't read config file %q: %s", path, err)
	}

	jsonContents, err := yaml.YAMLToJSON(yamlContents)
	if err != nil {
		logger.Infof("config content:\n%s", logger.Numerate(string(yamlContents)))
		logger.Fatalf("can't parse config file yaml %q: %s", path, err.Error())
	}

	json, err := simplejson.NewJson(jsonContents)
	if err != nil {
		logger.Fatalf("can't convert config to json %q: %s", path, err.Error())
	}

	err = applyEnvs(json)
	if err != nil {
		logger.Fatalf("can't get config values from environments: %s", err.Error())
	}

	config := parseConfig(json)
	if !config.Vault.ShouldUse {
		return config
	}

	vault, err := newVault(config.Vault.Address, config.Vault.Token)
	if err != nil {
		logger.Fatalf("can't create vault client: %s", err.Error())
	}

	for _, p := range config.Pipelines {
		applyVault(vault, p.Raw)
	}

	logger.Infof("config parsed, found %d pipelines", len(config.Pipelines))

	return config
}

func applyEnvs(json *simplejson.Json) error {
	for _, env := range os.Environ() {
		kv := strings.SplitN(env, "=", 2)
		if len(kv) != 2 {
			return errors.Errorf("can't parse env %s", env)
		}

		k, v := kv[0], kv[1]
		if strings.HasPrefix(k, "FILED_") {
			lower := strings.ToLower(k)
			path := strings.Split(lower, "_")[1:]
			json.SetPath(path, v)
		}
	}

	return nil
}

func parseConfig(json *simplejson.Json) *Config {
	config := NewConfig()
	vault := json.Get("vault")
	var err error

	addr := vault.Get("address")
	config.Vault.Address, err = addr.String()
	if err != nil {
		logger.Warnf("can't parse vault address: %s", err.Error())
	}

	token := vault.Get("token")
	config.Vault.Token, err = token.String()
	if err != nil {
		logger.Warnf("can't parse vault token: %s", err.Error())
	}
	config.Vault.ShouldUse = config.Vault.Address != "" && config.Vault.Token != ""

	pipelinesJson := json.Get("pipelines")
	pipelines := pipelinesJson.MustMap()
	if len(pipelines) == 0 {
		logger.Fatalf("no pipelines defined in config")
	}
	for i := range pipelines {
		raw := pipelinesJson.Get(i)
		config.Pipelines[i] = &PipelineConfig{Raw: raw}
	}

	return config
}

func applyVault(vault secreter, json *simplejson.Json) {
	if a, err := json.Array(); err == nil {
		for i := range a {
			field := json.GetIndex(i)
			if value, ok := tryGetSecret(vault, field); ok {
				a[i] = value

				continue
			}
			applyVault(vault, field)
		}
	}

	if m, err := json.Map(); err == nil {
		for k := range m {
			field := json.Get(k)
			if value, ok := tryGetSecret(vault, field); ok {
				json.Set(k, value)

				continue
			}
			applyVault(vault, field)
		}
	}
}

func tryGetSecret(vault secreter, field *simplejson.Json) (string, bool) {
	s, err := field.String()
	if err != nil {
		return "", false
	}

	// escape symbols.
	if strings.HasPrefix(s, `\vault(`) {
		s = strings.ReplaceAll(s, `\vault(`, "vault(")
		return s, true
	}

	if !strings.HasPrefix(s, "vault(") || !strings.HasSuffix(s, ")") {
		return "", false
	}

	args := strings.TrimPrefix(s, "vault(")
	args = strings.TrimSuffix(args, ")")
	noSpaces := strings.ReplaceAll(args, " ", "")
	pathAndKey := strings.Split(noSpaces, ",")

	logger.Infof("get secrets for %q and %q", pathAndKey[0], pathAndKey[1])
	secret, err := vault.GetSecret(pathAndKey[0], pathAndKey[1])
	if err != nil {
		logger.Fatalf("can't GetSecret: %s", err.Error())
	}

	logger.Infof("success getting secret %q and %q", pathAndKey[0], pathAndKey[1])
	return secret, true
}

// Parse holy shit! who write this function?
func Parse(ptr interface{}, values map[string]int) error {
	v := reflect.ValueOf(ptr).Elem()
	t := v.Type()

	if t.Kind() != reflect.Struct {
		return nil
	}

	childs := make([]reflect.Value, 0)
	for i := 0; i < t.NumField(); i++ {
		vField := v.Field(i)
		tField := t.Field(i)

		childTag := tField.Tag.Get("child")
		if childTag == "true" {
			childs = append(childs, vField)
			continue
		}

		sliceTag := tField.Tag.Get("slice")
		if sliceTag == "true" {
			if err := ParseSlice(vField, values); err != nil {
				return err
			}
			continue
		}

		err := ParseField(v, vField, tField, values)
		if err != nil {
			return err
		}
	}

	for _, child := range childs {
		if err := ParseChild(v, child, values); err != nil {
			return err
		}
	}

	return nil
}

// it isn't just a recursion
// it also captures values with the same name from parent
// i.e. take this config:
// {
// 	"T": 10,
// 	"Child": { // has `child:true` in a tag
// 		"T": null
// 	}
// }
// this function will set `config.Child.T = config.T`
// see file.d/cfg/config_test.go:TestHierarchy for an example
func ParseChild(parent reflect.Value, v reflect.Value, values map[string]int) error {
	if v.CanAddr() {
		for i := 0; i < v.NumField(); i++ {
			name := v.Type().Field(i).Name
			val := parent.FieldByName(name)
			if val.CanAddr() {
				v.Field(i).Set(val)
			}
		}

		err := Parse(v.Addr().Interface(), values)
		if err != nil {
			return err
		}
	}
	return nil
}

// ParseSlice recursively parses elements of an slice
// calls Parse, not ParseChild (!)
func ParseSlice(v reflect.Value, values map[string]int) error {
	for i := 0; i < v.Len(); i++ {
		if err := Parse(v.Index(i).Addr().Interface(), values); err != nil {
			return err
		}
	}
	return nil
}

func ParseField(v reflect.Value, vField reflect.Value, tField reflect.StructField, values map[string]int) error {
	tag := tField.Tag.Get("required")
	required := tag == "true"

	tag = tField.Tag.Get("default")
	if tag != "" {
		switch vField.Kind() {
		case reflect.String:
			if vField.String() == "" {
				vField.SetString(tag)
			}
		case reflect.Int:
			val, err := strconv.Atoi(tag)
			if err != nil {
				return errors.Wrapf(err, "default value for field %s should be int, got=%s", tField.Name, tag)
			}
			vField.SetInt(int64(val))
		case reflect.Slice:
			if vField.Len() == 0 {
				val := strings.Fields(tag)
				vField.Set(reflect.MakeSlice(vField.Type(), len(val), len(val)))
				for i, v := range val {
					vField.Index(i).SetString(v)
				}
			}
		}
	}

	tag = tField.Tag.Get("options")
	if tag != "" {
		parts := strings.Split(tag, "|")
		if vField.Kind() != reflect.String {
			return fmt.Errorf("options deals with strings only, but field %s has %s type", tField.Name, tField.Type.Name())
		}

		found := false
		for _, part := range parts {
			if vField.String() == part {
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf("field %s should be one of %s, got=%s", tField.Name, tag, vField.String())
		}
	}

	tag = tField.Tag.Get("parse")
	if tag != "" {
		if vField.Kind() != reflect.String {
			return fmt.Errorf("field %s should be a string, but it's %s", tField.Name, tField.Type.Name())
		}

		finalField := v.FieldByName(tField.Name + "_")

		switch tag {
		case "regexp":
			re, err := CompileRegex(vField.String())
			if err != nil {
				return fmt.Errorf("can't compile regexp for field %s: %s", tField.Name, err.Error())
			}
			finalField.Set(reflect.ValueOf(re))
		case "selector":
			fields := ParseFieldSelector(vField.String())
			finalField.Set(reflect.ValueOf(fields))
		case "duration":
			result, err := time.ParseDuration(vField.String())
			if err != nil {
				return fmt.Errorf("field %s has wrong duration format: %s", tField.Name, err.Error())
			}

			finalField.SetInt(int64(result))
		case "list-map":
			listMap := make(map[string]bool)

			parts := strings.Split(vField.String(), ",")
			for _, part := range parts {
				cleanPart := strings.TrimSpace(part)
				listMap[cleanPart] = true
			}

			finalField.Set(reflect.ValueOf(listMap))
		case "list":
			list := make([]string, 0)

			parts := strings.Split(vField.String(), ",")
			for _, part := range parts {
				cleanPart := strings.TrimSpace(part)
				list = append(list, cleanPart)
			}

			finalField.Set(reflect.ValueOf(list))
		case "expression":
			pos := strings.IndexAny(vField.String(), "*/+-")
			if pos == -1 {
				i, err := strconv.Atoi(vField.String())
				if err != nil {
					return fmt.Errorf("can't convert %s to int", vField.String())
				}
				finalField.SetInt(int64(i))
				return nil
			}

			op1 := strings.TrimSpace(vField.String()[:pos])
			op := vField.String()[pos]
			op2 := strings.TrimSpace(vField.String()[pos+1:])

			op1_, err := strconv.Atoi(op1)
			if err != nil {
				has := false
				op1_, has = values[op1]
				if !has {
					return fmt.Errorf("can't find value for %q in expression", op1)
				}
			}

			op2_, err := strconv.Atoi(op2)
			if err != nil {
				has := false
				op2_, has = values[op2]
				if !has {
					return fmt.Errorf("can't find value for %q in expression", op2)
				}
			}

			result := 0
			switch op {
			case '+':
				result = op1_ + op2_
			case '-':
				result = op1_ - op2_
			case '*':
				result = op1_ * op2_
			case '/':
				result = op1_ / op2_
			default:
				return fmt.Errorf("unknown operation %q", op)
			}

			finalField.SetInt(int64(result))

		case "base8":
			value, err := strconv.ParseInt(vField.String(), 8, 64)
			if err != nil {
				return fmt.Errorf("could not parse field %s, err: %s", tField.Name, err.Error())
			}
			finalField.SetInt(int64(value))

		default:
			return fmt.Errorf("unsupported parse type %q for field %s", tag, tField.Name)
		}
	}

	if required && vField.IsZero() {
		return fmt.Errorf("field %s should set as non-zero value", tField.Name)
	}

	return nil
}

func UnescapeMap(fields map[string]interface{}) map[string]string {
	result := make(map[string]string)

	for key, val := range fields {
		if len(key) == 0 {
			continue
		}

		if key[0] == '_' {
			key = key[1:]
		}

		result[key] = val.(string)
	}

	return result
}

func ParseFieldSelector(selector string) []string {
	result := make([]string, 0)
	tail := ""
	for {
		pos := strings.IndexByte(selector, '.')
		if pos == -1 {
			break
		}
		if pos > 0 && selector[pos-1] == '\\' {
			tail = tail + selector[:pos-1] + "."
			selector = selector[pos+1:]
			continue
		}

		if len(selector) > pos+1 {
			if selector[pos+1] == '.' {
				tail = selector[:pos+1]
				selector = selector[pos+2:]
				continue
			}
		}

		result = append(result, tail+selector[:pos])
		selector = selector[pos+1:]
		tail = ""
	}

	if len(selector)+len(tail) != 0 {
		result = append(result, tail+selector)
	}

	return result
}

func ListToMap(a []string) map[string]bool {
	result := make(map[string]bool)
	for _, key := range a {
		result[key] = true
	}

	return result
}

func CompileRegex(s string) (*regexp.Regexp, error) {
	if s == "" {
		return nil, fmt.Errorf(`regexp is empty`)
	}

	if len(s) == 0 || s[0] != '/' || s[len(s)-1] != '/' {
		return nil, fmt.Errorf(`regexp "%s" should be surounded by "/"`, s)
	}

	return regexp.Compile(s[1 : len(s)-1])
}
