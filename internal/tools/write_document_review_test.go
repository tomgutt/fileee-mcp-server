package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/strausmann/go-fileee/fileee"
)

func (f *fakeReviewDocumentService) Update(_ context.Context, doc *fileee.Document) (*fileee.Document, error) {
	f.updateCalls++
	f.updated = copyReviewDoc(doc)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if f.updateNil {
		return nil, nil
	}
	out := copyReviewDoc(doc)
	out.Modified = out.Modified.Add(time.Millisecond)
	f.current = copyReviewDoc(out)
	return out, nil
}

type reviewLookup[T any] struct {
	values map[string]*T
	err    error
	calls  []string
}

func (l *reviewLookup[T]) Get(_ context.Context, id string) (*T, error) {
	l.calls = append(l.calls, id)
	if l.err != nil {
		return nil, l.err
	}
	if value, ok := l.values[id]; ok {
		return value, nil
	}
	return nil, fileee.ErrNotFound
}

func testReviewRefs() reviewReferences {
	return reviewReferences{
		contacts: &reviewLookup[fileee.Contact]{values: map[string]*fileee.Contact{
			"c-self": {ID: "c-self", CompanyID: "company-self", ContactType: fileee.ContactTypeCompany, FromUserDB: true},
		}},
		tags: &reviewLookup[fileee.Tag]{values: map[string]*fileee.Tag{
			"t-old": {ID: "t-old", Name: "Existing"}, "t-good": {ID: "t-good", Name: "Suitable"},
		}},
		types: &reviewLookup[fileee.DocumentType]{values: map[string]*fileee.DocumentType{
			"bill":        {ID: "bill", I18NName: "Bill", DocumentTypeScheme: "bill-scheme"},
			"information": {ID: "information", I18NName: "Information", DocumentTypeScheme: "information-scheme"},
		}},
		schemes: &reviewLookup[fileee.DocumentTypeScheme]{values: map[string]*fileee.DocumentTypeScheme{
			"bill-scheme":        {ID: "bill-scheme", SchemaDefinition: fileee.SchemaField{ComposingTypes: []fileee.SchemaField{{Key: "amount"}, {Key: "issueDate"}, {Key: "futureAttribute"}}}},
			"information-scheme": {ID: "information-scheme"},
		}},
	}
}

func TestMetadataPatchPreservesOmissionsAndClearsNull(t *testing.T) {
	f := &fakeReviewDocumentService{current: reviewDoc(t)}
	refs := testReviewRefs()
	before, _ := f.current.Attributes.AttributeFields()
	_, out, err := patchDocumentMetadataFromService(context.Background(), f, refs, patchDocumentMetadataInput{ID: "d1", ExpectedModified: "2026-01-02T03:04:05.456Z", Fields: map[string]any{"amount": nil, "issueDate": nil, "receiverId": "c-self"}})
	if err != nil || !out.Changed || f.getCalls != 2 || f.updateCalls != 1 {
		t.Fatalf("patch: %+v %v; gets=%d updates=%d", out, err, f.getCalls, f.updateCalls)
	}
	after, _ := f.updated.Attributes.AttributeFields()
	if string(after["amount"]) != "null" || string(after["issueDate"]) != "null" || f.updated.Attributes.ReceiverID != "c-self" {
		t.Fatalf("null/contact changes = %s %s %s", after["amount"], after["issueDate"], after["receiverId"])
	}
	for key, value := range before {
		if key == "amount" || key == "issueDate" || key == "receiverId" {
			continue
		}
		var old, new any
		_ = json.Unmarshal(value, &old)
		_ = json.Unmarshal(after[key], &new)
		if !reflect.DeepEqual(old, new) {
			t.Fatalf("omitted field %s changed", key)
		}
	}
	contacts := refs.contacts.(*reviewLookup[fileee.Contact])
	if !reflect.DeepEqual(contacts.calls, []string{"c-self"}) || f.updated.Attributes.ReceiverID == "company-self" {
		t.Fatal("assignment must use an existing contact ID, independent of company ID/type")
	}
	encoded, _ := json.Marshal(f.updated)
	if !strings.Contains(string(encoded), `"future":{"keep":true}`) || !strings.Contains(string(encoded), `"rotation":90`) || !strings.Contains(string(encoded), `"source":"USER"`) {
		t.Fatalf("complete current document was not preserved: %s", encoded)
	}
}

func TestMetadataPatchMergesMoneyChildrenAndReplacesExistingTagIDs(t *testing.T) {
	f := &fakeReviewDocumentService{current: reviewDoc(t)}
	refs := testReviewRefs()
	_, _, err := patchDocumentMetadataFromService(context.Background(), f, refs, patchDocumentMetadataInput{ID: "d1", Fields: map[string]any{"amount": map[string]any{"value": 0.0}, "tagIds": []any{"t-good"}}})
	if err != nil {
		t.Fatal(err)
	}
	if f.updated.Attributes.Amount.Value != 0 || f.updated.Attributes.Amount.Currency != "EURO" || !reflect.DeepEqual(f.updated.Attributes.TagIDs, []string{"t-good"}) {
		t.Fatalf("money/tags = %+v %v", f.updated.Attributes.Amount, f.updated.Attributes.TagIDs)
	}
	fields, _ := f.updated.Attributes.AttributeFields()
	if !strings.Contains(string(fields["amount"]), `"enumClassName":"Currency"`) {
		t.Fatal("money update dropped the existing enum wrapper")
	}
	if !reflect.DeepEqual(refs.tags.(*reviewLookup[fileee.Tag]).calls, []string{"t-good"}) {
		t.Fatal("tag replacement must validate existing tag IDs without changing tag records")
	}
}

func TestMetadataPatchRejectsProtectedFieldsInvalidIDsAndReadOnlyCapabilities(t *testing.T) {
	for _, fields := range []map[string]any{
		{"read": true}, {"reviewed": true}, {"secured": false}, {"status": "DONE"},
		{"receiverId": "company-self"}, {"senderId": "../../other"}, {"tagIds": []any{"not-a-tag"}},
		{"tagIds": []any{"t-good", "t-good"}}, {"issueDate": "2026-02-31"}, {"amount": map[string]any{"oops": 1.0}},
	} {
		f := &fakeReviewDocumentService{current: reviewDoc(t)}
		_, _, err := patchDocumentMetadataFromService(context.Background(), f, testReviewRefs(), patchDocumentMetadataInput{ID: "d1", Fields: fields})
		if err == nil || f.updateCalls != 0 {
			t.Fatalf("invalid patch accepted: %v, %v", fields, err)
		}
	}
	for _, action := range []fileee.DocumentAction{fileee.ActionEdit, fileee.ActionEditTags} {
		f := &fakeReviewDocumentService{current: reviewDoc(t)}
		f.current.ForbiddenActions = []fileee.DocumentAction{action}
		_, _, err := patchDocumentMetadataFromService(context.Background(), f, testReviewRefs(), patchDocumentMetadataInput{ID: "d1", Fields: map[string]any{"tagIds": []any{"t-good"}}})
		if err == nil || f.updateCalls != 0 {
			t.Fatalf("forbidden action %s was ignored", action)
		}
	}
}

func TestArchiveChangesOnlyReadAndHonorsModifiedEvenWhenVersionIsZero(t *testing.T) {
	f := &fakeReviewDocumentService{current: reviewDoc(t)}
	_, out, err := setDocumentArchivedFromService(context.Background(), f, setDocumentArchivedInput{ID: "d1", Archived: true, ExpectedModified: "2026-01-02T03:04:05.456Z"})
	if err != nil || out.Archived == nil || !*out.Archived || out.InDashboard || *f.updated.Attributes.Reviewed || *f.updated.Attributes.Payed || f.updated.Status != fileee.DocumentStatusClassified {
		t.Fatalf("archive changed wrong state: %+v %v", out, err)
	}
	_, out, err = setDocumentArchivedFromService(context.Background(), f, setDocumentArchivedInput{ID: "d1", Archived: true})
	if err != nil || out.Changed || f.updateCalls != 1 {
		t.Fatalf("archive no-op: %+v %v", out, err)
	}
	_, out, err = setDocumentArchivedFromService(context.Background(), f, setDocumentArchivedInput{ID: "d1", Archived: false})
	if err != nil || *out.Archived || !out.InDashboard {
		t.Fatalf("unarchive: %+v %v", out, err)
	}
	for _, changing := range []bool{false, true} {
		f := &fakeReviewDocumentService{current: reviewDoc(t)}
		in := setDocumentArchivedInput{ID: "d1", Archived: true, ExpectedModified: "2026-01-02T03:04:05.455Z"}
		if changing {
			in.ExpectedModified = "2026-01-02T03:04:05.456Z"
			f.secondRead = reviewDoc(t)
			f.secondRead.Modified = f.secondRead.Modified.Add(time.Millisecond)
		}
		_, _, err := setDocumentArchivedFromService(context.Background(), f, in)
		if err == nil || f.updateCalls != 0 {
			t.Fatal("stale modified state was accepted despite version remaining zero")
		}
	}
}

func TestReviewMutationPreservesBackendAndNetworkErrors(t *testing.T) {
	for _, backendErr := range []error{&fileee.APIError{HTTPStatus: http.StatusBadRequest}, &fileee.APIError{HTTPStatus: http.StatusInternalServerError}, context.DeadlineExceeded} {
		for _, stage := range []string{"get", "update"} {
			for _, operation := range []string{"patch", "archive"} {
				f := &fakeReviewDocumentService{current: reviewDoc(t)}
				if stage == "get" {
					f.getErr = backendErr
				} else {
					f.updateErr = backendErr
				}
				var err error
				if operation == "patch" {
					_, _, err = patchDocumentMetadataFromService(context.Background(), f, testReviewRefs(), patchDocumentMetadataInput{ID: "d1", Fields: map[string]any{"title": "Changed"}})
				} else {
					_, _, err = setDocumentArchivedFromService(context.Background(), f, setDocumentArchivedInput{ID: "d1", Archived: true})
				}
				if !errors.Is(err, backendErr) {
					t.Fatalf("%s/%s did not preserve error: %v", operation, stage, err)
				}
			}
		}
	}
}

func TestReviewWriteHandlersRejectInvalidInputBeforeClientResolution(t *testing.T) {
	_, _, err := patchDocumentMetadataHandler(nil, discardLogger())(context.Background(), nil, patchDocumentMetadataInput{ID: "d1", Fields: map[string]any{"read": true}})
	if err == nil {
		t.Fatal("protected field accepted")
	}
	_, _, err = setDocumentArchivedHandler(nil, discardLogger())(context.Background(), nil, setDocumentArchivedInput{ID: "d1", ExpectedModified: "invalid"})
	if err == nil {
		t.Fatal("invalid modified timestamp accepted")
	}
}
