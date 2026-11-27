package tools

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/strausmann/go-fileee/fileee"
)

func TestReviewCustomFieldsRespectCurrentSchemaAndRecursivePermissions(t *testing.T) {
	f := &fakeReviewDocumentService{current: reviewDoc(t)}
	refs := testReviewRefs()
	zero := int64(0)
	_, _, err := patchDocumentMetadataFromService(context.Background(), f, refs, patchDocumentMetadataInput{ID: "d1", ExpectedVersion: &zero, Fields: map[string]any{"futureAttribute": "Corrected custom text", "netIncome": map[string]any{"currency": "EURO", "value": -2.5}}})
	if err != nil || f.updated.Attributes.NetIncome == nil || f.updated.Attributes.NetIncome.Value != -2.5 {
		t.Fatalf("custom/new group patch: %+v %v", f.updated, err)
	}
	fields, _ := f.updated.Attributes.AttributeFields()
	if !strings.Contains(string(fields["futureAttribute"]), "Corrected custom text") || !strings.Contains(string(fields["netIncome"]), `"attributeGroup":"COMPOSED"`) {
		t.Fatal("custom field or new composed wrapper missing")
	}
	group := json.RawMessage(`{"type":"COMPOSED","data":{"label":{"type":"TEXT","value":"Old"},"enabled":{"type":"BOOLEAN","value":false},"count":{"type":"INTEGER","value":2}}}`)
	if err := f.current.Attributes.PatchAttributeFields(map[string]json.RawMessage{"customGroup": group}); err != nil {
		t.Fatal(err)
	}
	scheme := refs.schemes.(*reviewLookup[fileee.DocumentTypeScheme]).values["bill-scheme"]
	scheme.SchemaDefinition.ComposingTypes = append(scheme.SchemaDefinition.ComposingTypes, fileee.SchemaField{Key: "customGroup", ComposingTypes: []fileee.SchemaField{{Key: "label"}, {Key: "count", ReadOnly: true}}})
	_, _, err = patchDocumentMetadataFromService(context.Background(), f, refs, patchDocumentMetadataInput{ID: "d1", Fields: map[string]any{"customGroup": map[string]any{"label": "New", "enabled": true}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{nil, 3.0} {
		puts := f.updateCalls
		_, _, err := patchDocumentMetadataFromService(context.Background(), f, refs, patchDocumentMetadataInput{ID: "d1", Fields: map[string]any{"customGroup": map[string]any{"count": value}}})
		if err == nil || f.updateCalls != puts {
			t.Fatal("recursive read-only field was changed or cleared")
		}
	}
	scheme.SchemaDefinition.ComposingTypes[2].ReadOnly = true
	puts := f.updateCalls
	_, _, err = patchDocumentMetadataFromService(context.Background(), f, refs, patchDocumentMetadataInput{ID: "d1", Fields: map[string]any{"futureAttribute": nil}})
	if err == nil || f.updateCalls != puts {
		t.Fatal("read-only custom field was cleared")
	}

}

func TestReviewAttributeValidationRejectsInvalidValuesAndUnknownWrappers(t *testing.T) {
	for _, test := range []struct {
		raw       string
		value     any
		wantError bool
	}{
		{`{"type":"INTEGER","value":2}`, 3.0, false},
		{`{"type":"INTEGER","value":2}`, 3.5, true},
		{`{"type":"DOUBLE","value":2}`, math.NaN(), true},
		{`{"type":"DOUBLE","value":2}`, "3", true},
		{`{"type":"TEXT","value":"old"}`, 3.0, true},
		{`{"type":"BOOLEAN","value":false}`, "true", true},
		{`{"type":"DATE","value":"2026-01-01"}`, "2026-02-31", true},
		{`{"type":"ENUMERATION","enumClassName":"Language","value":"DE"}`, "EN", false},
		{`{"type":"ENUMERATION","enumClassName":"Language","value":"DE"}`, " ", true},
		{`{"type":"LIST","containedType":"TEXT","value":[]}`, []any{3.0}, true},
		{`{"type":"LIST","containedType":"TEXT","value":[]}`, "wrong", true},
		{`{"type":"LIST","containedType":"TEXT","value":[]}`, make([]string, 101), true},
		{`{"type":"COMPOSED","data":{"value":{"type":"DOUBLE","value":2}}}`, map[string]any{"value": nil}, false},
		{`{"type":"COMPOSED","data":{"value":{"type":"DOUBLE","value":2}}}`, "wrong", true},
	} {
		spec, err := inferReviewField(json.RawMessage(test.raw))
		if err != nil {
			t.Fatal(err)
		}
		_, err = buildReviewAttribute(json.RawMessage(test.raw), test.value, spec)
		if (err != nil) != test.wantError {
			t.Fatalf("%s with %v: %v", test.raw, test.value, err)
		}
	}
	for _, raw := range []string{`{"type":"UNKNOWN"}`, `{"type":"LIST","containedType":"INTEGER"}`, `{"type":"COMPOSED","data":{"value":{"type":"UNKNOWN"}}}`} {
		if _, err := inferReviewField(json.RawMessage(raw)); err == nil {
			t.Fatalf("unsupported wrapper accepted: %s", raw)
		}
	}
	if _, err := buildReviewAttribute(json.RawMessage(`{"type":"ENUMERATION","enumClassName":"Language","value":"DE"}`), "EN", reviewFieldSpec{kind: "TEXT"}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewPatchTypeClearsNoOpAndPreflightErrors(t *testing.T) {
	f := &fakeReviewDocumentService{current: reviewDoc(t)}
	_, _, err := patchDocumentMetadataFromService(context.Background(), f, testReviewRefs(), patchDocumentMetadataInput{ID: "d1", Fields: map[string]any{"documentTypeId": nil}})
	if err != nil || f.current.Attributes.DocumentTypeID != "" {
		t.Fatalf("type null clear: %v", err)
	}
	_, _, err = patchDocumentMetadataFromService(context.Background(), f, testReviewRefs(), patchDocumentMetadataInput{ID: "d1", Fields: map[string]any{"title": "Same title"}})
	if err != nil {
		t.Fatal(err)
	}
	puts := f.updateCalls
	_, out, err := patchDocumentMetadataFromService(context.Background(), f, testReviewRefs(), patchDocumentMetadataInput{ID: "d1", Fields: map[string]any{"title": "Same title"}})
	if err != nil || out.Changed || f.updateCalls != puts {
		t.Fatal("identical patch was not a no-op")
	}
	for _, in := range []patchDocumentMetadataInput{
		{ID: "../bad", Fields: map[string]any{"title": "x"}}, {ID: "d1"},
		{ID: "d1", Fields: map[string]any{"title": "x"}, ExpectedModified: "invalid"},
	} {
		if _, _, err := patchDocumentMetadataFromService(context.Background(), f, testReviewRefs(), in); err == nil {
			t.Fatal("invalid patch preflight accepted")
		}
	}
	badVersion := int64(-1)
	if validateReviewExpectations(&badVersion, "") == nil {
		t.Fatal("negative version accepted")
	}
	wrongVersion := int64(1)
	if checkReviewCurrent(reviewDoc(t), "d1", &wrongVersion, "") == nil || checkReviewCurrent(nil, "d1", nil, "") == nil {
		t.Fatal("missing/stale current document accepted")
	}
	deleted := reviewDoc(t)
	deleted.Deleted = true
	if checkReviewCurrent(deleted, "d1", nil, "") == nil {
		t.Fatal("deleted document accepted")
	}
	for _, modify := range []func(reviewReferences){
		func(r reviewReferences) { r.types.(*reviewLookup[fileee.DocumentType]).err = context.DeadlineExceeded },
		func(r reviewReferences) { r.types.(*reviewLookup[fileee.DocumentType]).values["bill"].Deleted = true },
		func(r reviewReferences) {
			r.schemes.(*reviewLookup[fileee.DocumentTypeScheme]).err = context.DeadlineExceeded
		},
		func(r reviewReferences) {
			r.schemes.(*reviewLookup[fileee.DocumentTypeScheme]).values["bill-scheme"].Deleted = true
		},
	} {
		refs := testReviewRefs()
		modify(refs)
		f := &fakeReviewDocumentService{current: reviewDoc(t)}
		_, _, err := patchDocumentMetadataFromService(context.Background(), f, refs, patchDocumentMetadataInput{ID: "d1", Fields: map[string]any{"title": "Changed"}})
		if err == nil || f.updateCalls != 0 {
			t.Fatal("invalid reference/schema still issued an update")
		}
	}
	f = &fakeReviewDocumentService{current: reviewDoc(t), updateNil: true}
	_, _, err = patchDocumentMetadataFromService(context.Background(), f, testReviewRefs(), patchDocumentMetadataInput{ID: "d1", Fields: map[string]any{"title": "Changed"}})
	if err == nil {
		t.Fatal("ambiguous save response accepted")
	}
	for _, run := range []func() error{
		func() error {
			_, _, err := patchDocumentMetadataHandler(nil, discardLogger())(context.Background(), nil, patchDocumentMetadataInput{ID: "d1", Fields: map[string]any{"title": "x"}})
			return err
		},
		func() error {
			_, _, err := setDocumentArchivedHandler(nil, discardLogger())(context.Background(), nil, setDocumentArchivedInput{ID: "d1", Archived: true})
			return err
		},
	} {
		if run() == nil {
			t.Fatal("missing identity accepted")
		}
	}
}
