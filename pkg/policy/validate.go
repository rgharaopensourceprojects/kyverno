package policy

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/nirmata/kyverno/pkg/openapi"

	kyverno "github.com/nirmata/kyverno/pkg/api/kyverno/v1"
	"github.com/nirmata/kyverno/pkg/engine/anchor"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Validate does some initial check to verify some conditions
// - One operation per rule
// - ResourceDescription mandatory checks
func Validate(p kyverno.ClusterPolicy) error {
	if path, err := validateUniqueRuleName(p); err != nil {
		return fmt.Errorf("path: spec.%s: %v", path, err)
	}
	if p.Spec.Background == nil {
		//skipped policy mutation default -> skip validation -> will not be processed for background processing
		return nil
	}
	if *p.Spec.Background {
		if err := ContainsUserInfo(p); err != nil {
			// policy.spec.background -> "true"
			// - cannot use variables with request.userInfo
			// - cannot define userInfo(roles, cluserRoles, subjects) for filtering (match & exclude)
			return fmt.Errorf("failed at %v. User info related conditions are not allowed in background mode. "+
				"If you would like to use user info related conditions kindly disable background mode for this policy by "+
				"setting spec/background to false", err)
		}
	}

	for i, rule := range p.Spec.Rules {
		// only one type of rule is allowed per rule
		if err := validateRuleType(rule); err != nil {
			return fmt.Errorf("path: spec.rules[%d]: %v", i, err)
		}

		// validate resource description
		if path, err := validateResources(rule); err != nil {
			return fmt.Errorf("path: spec.rules[%d].%s: %v", i, path, err)
		}
		// validate rule types
		// only one type of rule is allowed per rule
		if err := validateRuleType(rule); err != nil {
			// as there are more than 1 operation in rule, not need to evaluate it further
			return fmt.Errorf("path: spec.rules[%d]: %v", i, err)
		}
		// Operation Validation
		// Mutation
		if rule.HasMutate() {
			if path, err := validateMutation(rule.Mutation); err != nil {
				return fmt.Errorf("path: spec.rules[%d].mutate.%s.: %v", i, path, err)
			}
		}
		// Validation
		if rule.HasValidate() {
			if path, err := validateValidation(rule.Validation); err != nil {
				return fmt.Errorf("path: spec.rules[%d].validate.%s.: %v", i, path, err)
			}
		}
		// Generation
		if rule.HasGenerate() {
			if path, err := validateGeneration(rule.Generation); err != nil {
				return fmt.Errorf("path: spec.rules[%d].generate.%s.: %v", i, path, err)
			}
		}
	}

	if err := openapi.ValidatePolicyMutation(p); err != nil {
		return fmt.Errorf("Failed to validate policy: %v", err)
	}

	return nil
}

func validateResources(rule kyverno.Rule) (string, error) {
	// validate userInfo in match and exclude
	if path, err := validateUserInfo(rule); err != nil {
		return fmt.Sprintf("resources.%s", path), err
	}

	// matched resources
	if path, err := validateMatchedResourceDescription(rule.MatchResources.ResourceDescription); err != nil {
		return fmt.Sprintf("resources.%s", path), err
	}
	// exclude resources
	if path, err := validateExcludeResourceDescription(rule.ExcludeResources.ResourceDescription); err != nil {
		return fmt.Sprintf("resources.%s", path), err
	}
	return "", nil
}

// ValidateUniqueRuleName checks if the rule names are unique across a policy
func validateUniqueRuleName(p kyverno.ClusterPolicy) (string, error) {
	var ruleNames []string

	for i, rule := range p.Spec.Rules {
		if containString(ruleNames, rule.Name) {
			return fmt.Sprintf("rule[%d]", i), fmt.Errorf(`duplicate rule name: '%s'`, rule.Name)
		}
		ruleNames = append(ruleNames, rule.Name)
	}
	return "", nil
}

// validateRuleType checks only one type of rule is defined per rule
func validateRuleType(r kyverno.Rule) error {
	ruleTypes := []bool{r.HasMutate(), r.HasValidate(), r.HasGenerate()}

	operationCount := func() int {
		count := 0
		for _, v := range ruleTypes {
			if v {
				count++
			}
		}
		return count
	}()

	if operationCount == 0 {
		return fmt.Errorf("no operation defined in the rule '%s'.(supported operations: mutation,validation,generation)", r.Name)
	} else if operationCount != 1 {
		return fmt.Errorf("multiple operations defined in the rule '%s', only one type of operation is allowed per rule", r.Name)
	}
	return nil
}

// validateResourceDescription checks if all necesarry fields are present and have values. Also checks a Selector.
// field type is checked through openapi
// Returns error if
// - kinds is empty array in matched resource block, i.e. kinds: []
// - selector is invalid
func validateMatchedResourceDescription(rd kyverno.ResourceDescription) (string, error) {
	if reflect.DeepEqual(rd, kyverno.ResourceDescription{}) {
		return "", fmt.Errorf("match resources not specified")
	}

	if err := validateResourceDescription(rd); err != nil {
		return "match", err
	}

	return "", nil
}

func validateUserInfo(rule kyverno.Rule) (string, error) {
	if err := validateRoles(rule.MatchResources.Roles); err != nil {
		return "match.roles", err
	}

	if err := validateSubjects(rule.MatchResources.Subjects); err != nil {
		return "match.subjects", err
	}

	if err := validateRoles(rule.ExcludeResources.Roles); err != nil {
		return "exclude.roles", err
	}

	if err := validateSubjects(rule.ExcludeResources.Subjects); err != nil {
		return "exclude.subjects", err
	}

	return "", nil
}

// a role must in format namespace:name
func validateRoles(roles []string) error {
	if len(roles) == 0 {
		return nil
	}

	for _, r := range roles {
		role := strings.Split(r, ":")
		if len(role) != 2 {
			return fmt.Errorf("invalid role %s, expect namespace:name", r)
		}
	}
	return nil
}

// a namespace should be set in kind ServiceAccount of a subject
func validateSubjects(subjects []rbacv1.Subject) error {
	if len(subjects) == 0 {
		return nil
	}

	for _, subject := range subjects {
		if subject.Kind == "ServiceAccount" {
			if subject.Namespace == "" {
				return fmt.Errorf("service account %s in subject expects a namespace", subject.Name)
			}
		}
	}
	return nil
}

func validateExcludeResourceDescription(rd kyverno.ResourceDescription) (string, error) {
	if reflect.DeepEqual(rd, kyverno.ResourceDescription{}) {
		// exclude is not mandatory
		return "", nil
	}
	if err := validateResourceDescription(rd); err != nil {
		return "exclude", err
	}
	return "", nil
}

// validateResourceDescription returns error if selector is invalid
// field type is checked through openapi
func validateResourceDescription(rd kyverno.ResourceDescription) error {
	if rd.Selector != nil {
		selector, err := metav1.LabelSelectorAsSelector(rd.Selector)
		if err != nil {
			return err
		}
		requirements, _ := selector.Requirements()
		if len(requirements) == 0 {
			return errors.New("the requirements are not specified in selector")
		}
	}
	return nil
}

func validateMutation(m kyverno.Mutation) (string, error) {
	// JSON Patches
	if len(m.Patches) != 0 {
		for i, patch := range m.Patches {
			if err := validatePatch(patch); err != nil {
				return fmt.Sprintf("patch[%d]", i), err
			}
		}
	}
	// Overlay
	if m.Overlay != nil {
		path, err := validatePattern(m.Overlay, "/", []anchor.IsAnchor{anchor.IsConditionAnchor, anchor.IsAddingAnchor})
		if err != nil {
			return path, err
		}
	}
	return "", nil
}

// Validate if all mandatory PolicyPatch fields are set
func validatePatch(pp kyverno.Patch) error {
	if pp.Path == "" {
		return errors.New("JSONPatch field 'path' is mandatory")
	}
	if pp.Operation == "add" || pp.Operation == "replace" {
		if pp.Value == nil {
			return fmt.Errorf("JSONPatch field 'value' is mandatory for operation '%s'", pp.Operation)
		}

		return nil
	} else if pp.Operation == "remove" {
		return nil
	}

	return fmt.Errorf("Unsupported JSONPatch operation '%s'", pp.Operation)
}

func validateValidation(v kyverno.Validation) (string, error) {
	if err := validateOverlayPattern(v); err != nil {
		// no need to proceed ahead
		return "", err
	}

	if v.Pattern != nil {
		if path, err := validatePattern(v.Pattern, "/", []anchor.IsAnchor{anchor.IsConditionAnchor, anchor.IsExistenceAnchor, anchor.IsEqualityAnchor, anchor.IsNegationAnchor}); err != nil {
			return fmt.Sprintf("pattern.%s", path), err
		}
	}

	if len(v.AnyPattern) != 0 {
		for i, pattern := range v.AnyPattern {
			if path, err := validatePattern(pattern, "/", []anchor.IsAnchor{anchor.IsConditionAnchor, anchor.IsExistenceAnchor, anchor.IsEqualityAnchor, anchor.IsNegationAnchor}); err != nil {
				return fmt.Sprintf("anyPattern[%d].%s", i, path), err
			}
		}
	}
	return "", nil
}

// validateOverlayPattern checks one of pattern/anyPattern must exist
func validateOverlayPattern(v kyverno.Validation) error {
	if v.Pattern == nil && len(v.AnyPattern) == 0 {
		return fmt.Errorf("a pattern or anyPattern must be specified")
	}

	if v.Pattern != nil && len(v.AnyPattern) != 0 {
		return fmt.Errorf("only one operation allowed per validation rule(pattern or anyPattern)")
	}

	return nil
}

// Validate returns error if generator is configured incompletely
func validateGeneration(gen kyverno.Generation) (string, error) {

	if gen.Data == nil && gen.Clone == (kyverno.CloneFrom{}) {
		return "", fmt.Errorf("clone or data are required")
	}
	if gen.Data != nil && gen.Clone != (kyverno.CloneFrom{}) {
		return "", fmt.Errorf("only one operation allowed per generate rule(data or clone)")
	}
	// check kind is non empty
	// check name is non empty
	if gen.Name == "" {
		return "name", fmt.Errorf("name cannot be empty")
	}
	if gen.Kind == "" {
		return "kind", fmt.Errorf("kind cannot be empty")
	}
	if !reflect.DeepEqual(gen.Clone, kyverno.CloneFrom{}) {
		if path, err := validateClone(gen.Clone); err != nil {
			return fmt.Sprintf("clone.%s", path), err
		}
	}
	if gen.Data != nil {
		//TODO: is this required ?? as anchors can only be on pattern and not resource
		// we can add this check by not sure if its needed here
		if path, err := validatePattern(gen.Data, "/", []anchor.IsAnchor{}); err != nil {
			return fmt.Sprintf("data.%s", path), fmt.Errorf("anchors not supported on generate resources: %v", err)
		}
	}
	return "", nil
}

func validateClone(c kyverno.CloneFrom) (string, error) {
	if c.Name == "" {
		return "name", fmt.Errorf("name cannot be empty")
	}
	if c.Namespace == "" {
		return "namespace", fmt.Errorf("namespace cannot be empty")
	}
	return "", nil
}

func validatePattern(patternElement interface{}, path string, supportedAnchors []anchor.IsAnchor) (string, error) {
	switch typedPatternElement := patternElement.(type) {
	case map[string]interface{}:
		return validateMap(typedPatternElement, path, supportedAnchors)
	case []interface{}:
		return validateArray(typedPatternElement, path, supportedAnchors)
	case string, float64, int, int64, bool, nil:
		//TODO? check operator
		return "", nil
	default:
		return path, fmt.Errorf("Validation rule failed at '%s', pattern contains unknown type", path)
	}
}

func validateMap(patternMap map[string]interface{}, path string, supportedAnchors []anchor.IsAnchor) (string, error) {
	// check if anchors are defined
	for key, value := range patternMap {
		// if key is anchor
		// check regex () -> this is anchor
		// ()
		// single char ()
		re, err := regexp.Compile(`^.?\(.+\)$`)
		if err != nil {
			return path + "/" + key, fmt.Errorf("Unable to parse the field %s: %v", key, err)
		}

		matched := re.MatchString(key)
		// check the type of anchor
		if matched {
			// some type of anchor
			// check if valid anchor
			if !checkAnchors(key, supportedAnchors) {
				return path + "/" + key, fmt.Errorf("Unsupported anchor %s", key)
			}

			// addition check for existence anchor
			// value must be of type list
			if anchor.IsExistenceAnchor(key) {
				typedValue, ok := value.([]interface{})
				if !ok {
					return path + "/" + key, fmt.Errorf("Existence anchor should have value of type list")
				}
				// validate there is only one entry in the list
				if len(typedValue) == 0 || len(typedValue) > 1 {
					return path + "/" + key, fmt.Errorf("Existence anchor: single value expected, multiple specified")
				}
			}
		}
		// lets validate the values now :)
		if errPath, err := validatePattern(value, path+"/"+key, supportedAnchors); err != nil {
			return errPath, err
		}
	}
	return "", nil
}

func validateArray(patternArray []interface{}, path string, supportedAnchors []anchor.IsAnchor) (string, error) {
	for i, patternElement := range patternArray {
		currentPath := path + strconv.Itoa(i) + "/"
		// lets validate the values now :)
		if errPath, err := validatePattern(patternElement, currentPath, supportedAnchors); err != nil {
			return errPath, err
		}
	}
	return "", nil
}

func checkAnchors(key string, supportedAnchors []anchor.IsAnchor) bool {
	for _, f := range supportedAnchors {
		if f(key) {
			return true
		}
	}
	return false
}
