package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/strausmann/gangway/serve"
	"github.com/strausmann/go-fileee/fileee"
)

type reviewReferences struct {
	contacts reviewGetter[fileee.Contact]
	tags     reviewGetter[fileee.Tag]
	types    reviewGetter[fileee.DocumentType]
	schemes  reviewGetter[fileee.DocumentTypeScheme]
}

func reviewReferencesFor(c *fileee.Client) reviewReferences {
	return reviewReferences{contacts: c.Contacts, tags: c.Tags, types: c.DocumentTypes, schemes: c.DocumentTypeSchemes}
}

type reviewFieldSpec struct {
	kind      string
	enumClass string
	children  map[string]reviewFieldSpec
	readOnly  bool
}

var reviewMetadataFields = map[string]reviewFieldSpec{
	"title": {kind: "TEXT"}, "documentTypeId": {kind: "TEXT"}, "senderId": {kind: "TEXT"}, "receiverId": {kind: "TEXT"},
	"issueDate": {kind: "DATE"}, "invoiceDate": {kind: "DATE"}, "invoiceDueDate": {kind: "DATE"},
	"invoiceId": {kind: "TEXT"}, "customerId": {kind: "TEXT"}, "paymentReference": {kind: "TEXT"},
	"contentLanguage": {kind: "TEXT"}, "payed": {kind: "BOOLEAN"}, "tagIds": {kind: "LIST"},
	"amount": reviewMoneySpec(), "grossIncome": reviewMoneySpec(), "netIncome": reviewMoneySpec(),
	"bankAccount1": {kind: "COMPOSED", children: map[string]reviewFieldSpec{
		"iban": {kind: "TEXT"}, "bic": {kind: "TEXT"}, "bank": {kind: "TEXT"}, "account_holder": {kind: "TEXT"},
	}},
}

func reviewMoneySpec() reviewFieldSpec {
	return reviewFieldSpec{kind: "COMPOSED", children: map[string]reviewFieldSpec{
		"currency": {kind: "ENUMERATION", enumClass: "Currency"}, "value": {kind: "DOUBLE"},
	}}
}

var reviewProtectedFields = map[string]bool{
	"read": true, "reviewed": true, "secured": true, "totalPageCount": true, "maxPageNr": true,
	"status": true, "deleted": true, "version": true, "created": true, "modified": true,
	"pages": true, "uploadAttribute": true, "sharedSpaceIds": true, "shareInformation": true, "forbiddenActions": true,
	"reminderIds": true,
}

func reviewPatchKeys(fields map[string]any) ([]string, error) {
	if len(fields) == 0 || len(fields) > 64 {
		return nil, fmt.Errorf("fields must contain between 1 and 64 metadata keys")
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		if !reviewIDPattern.MatchString(key) || reviewProtectedFields[key] {
			return nil, fmt.Errorf("fields contains a protected or invalid metadata key; use set_document_archived for archive state")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

func validateReviewExpectations(version *int64, modified string) error {
	if version != nil && *version < 0 {
		return fmt.Errorf("expectedVersion must not be negative")
	}
	if modified != "" {
		if _, err := time.Parse(time.RFC3339Nano, modified); err != nil {
			return fmt.Errorf("expectedModified must be an RFC3339 timestamp from get_document_review")
		}
	}
	return nil
}

func checkReviewCurrent(doc *fileee.Document, id string, version *int64, modified string) error {
	if doc == nil || doc.ID != id {
		return fmt.Errorf("Fileee returned no matching current document")
	}
	if version != nil && *version != doc.Version {
		return fmt.Errorf("document changed: expectedVersion does not match; reload get_document_review")
	}
	if modified != "" {
		expected, _ := time.Parse(time.RFC3339Nano, modified)
		if !expected.Equal(doc.Modified) {
			return fmt.Errorf("document changed: expectedModified does not match; reload get_document_review")
		}
	}
	return reviewCanEdit(doc)
}

// Stripes bound memory and serialize this process's read/validate/save sequence
// for the same caller and document. External actors still require the fresh
// second read and Fileee's own optimistic locking; no atomic CAS is claimed.
var reviewWriteStripes [64]sync.Mutex

func lockReviewWrite(ctx context.Context, id string) func() {
	h := fnv.New64a()
	if identity, ok := serve.IdentityFrom(ctx); ok && identity != nil {
		_, _ = h.Write([]byte(identity.Subject))
	}
	_, _ = h.Write([]byte("\x00" + id))
	lock := &reviewWriteStripes[h.Sum64()%uint64(len(reviewWriteStripes))]
	lock.Lock()
	return lock.Unlock
}

func inferReviewField(raw json.RawMessage) (reviewFieldSpec, error) {
	wrapper := decodeReviewWrapper(raw)
	spec := reviewFieldSpec{kind: reviewWrapperString(wrapper, "type"), enumClass: reviewWrapperString(wrapper, "enumClassName")}
	switch spec.kind {
	case "TEXT", "DATE", "BOOLEAN", "DOUBLE", "INTEGER", "ENUMERATION":
	case "LIST":
		if reviewWrapperString(wrapper, "containedType") != "TEXT" {
			return reviewFieldSpec{}, fmt.Errorf("only text-list metadata fields are supported")
		}
	case "COMPOSED":
		spec.children = make(map[string]reviewFieldSpec)
		for key, child := range decodeReviewWrapper(wrapper["data"]) {
			childSpec, err := inferReviewField(child)
			if err != nil {
				return reviewFieldSpec{}, err
			}
			spec.children[key] = childSpec
		}
	default:
		return reviewFieldSpec{}, fmt.Errorf("metadata field has an unsupported or absent attribute type")
	}
	return spec, nil
}

func reviewStringList(value any) ([]string, error) {
	var values []string
	switch list := value.(type) {
	case []string:
		values = append([]string{}, list...)
	case []any:
		values = make([]string, 0, len(list))
		for _, item := range list {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("list metadata requires an array of strings")
			}
			values = append(values, text)
		}
	default:
		return nil, fmt.Errorf("list metadata requires an array of strings")
	}
	if len(values) > maxLimit {
		return nil, fmt.Errorf("list metadata exceeds the limit of 100 entries")
	}
	return values, nil
}

func buildReviewAttribute(current json.RawMessage, value any, spec reviewFieldSpec) (json.RawMessage, error) {
	if spec.readOnly {
		return nil, fmt.Errorf("document-type scheme marks this metadata field read-only or server-only")
	}
	if value == nil {
		return json.RawMessage("null"), nil
	}
	wrapper := decodeReviewWrapper(current)
	if spec.kind == "TEXT" && reviewWrapperString(wrapper, "type") == "ENUMERATION" {
		spec.kind = "ENUMERATION"
		spec.enumClass = reviewWrapperString(wrapper, "enumClassName")
	}
	var wireValue any = value
	switch spec.kind {
	case "TEXT", "ENUMERATION", "DATE":
		text, ok := value.(string)
		if !ok || len(text) > 16384 {
			return nil, fmt.Errorf("text/date/enum metadata requires a string of at most 16384 bytes")
		}
		if spec.kind == "DATE" {
			if _, err := time.Parse("2006-01-02", text); err != nil {
				return nil, fmt.Errorf("date metadata requires a valid YYYY-MM-DD date")
			}
		}
		if spec.kind == "ENUMERATION" && strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("enum metadata requires a non-empty Fileee enum value")
		}
	case "BOOLEAN":
		if _, ok := value.(bool); !ok {
			return nil, fmt.Errorf("boolean metadata requires true or false")
		}
	case "DOUBLE", "INTEGER":
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("numeric metadata requires a finite number")
		}
		var number float64
		if err := json.Unmarshal(encoded, &number); err != nil || math.IsNaN(number) || math.IsInf(number, 0) || (spec.kind == "INTEGER" && math.Trunc(number) != number) {
			return nil, fmt.Errorf("numeric metadata requires a finite number of the correct kind")
		}
	case "LIST":
		values, err := reviewStringList(value)
		if err != nil {
			return nil, err
		}
		wireValue = values
		wrapper["containedType"] = json.RawMessage(`"TEXT"`)
	case "COMPOSED":
		values, ok := value.(map[string]any)
		if !ok || len(values) == 0 {
			return nil, fmt.Errorf("composed metadata requires a non-empty object of subfield values")
		}
		data := decodeReviewWrapper(wrapper["data"])
		for key, childValue := range values {
			childSpec, ok := spec.children[key]
			if !ok {
				return nil, fmt.Errorf("composed metadata contains an unsupported subfield")
			}
			child, err := buildReviewAttribute(data[key], childValue, childSpec)
			if err != nil {
				return nil, err
			}
			data[key] = child
		}
		encoded, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		wrapper["data"] = encoded
		delete(wrapper, "value")
		if _, ok := wrapper["attributeGroup"]; !ok {
			wrapper["attributeGroup"] = json.RawMessage(`"COMPOSED"`)
		}
	default:
		return nil, fmt.Errorf("unsupported metadata attribute kind")
	}
	if spec.kind != "COMPOSED" {
		encoded, err := json.Marshal(wireValue)
		if err != nil {
			return nil, err
		}
		wrapper["value"] = encoded
	}
	wrapper["type"], _ = json.Marshal(spec.kind)
	wrapper["source"] = json.RawMessage(`"USER"`)
	if spec.enumClass != "" {
		wrapper["enumClassName"], _ = json.Marshal(spec.enumClass)
	}
	return json.Marshal(wrapper)
}
