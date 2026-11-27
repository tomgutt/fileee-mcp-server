package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strausmann/fileee-mcp-server/internal/clientpool"
	"github.com/strausmann/go-fileee/fileee"
)

type reviewDocumentWriteService interface {
	reviewGetter[fileee.Document]
	Update(context.Context, *fileee.Document) (*fileee.Document, error)
}

type patchDocumentMetadataInput struct {
	ID               string         `json:"id"`
	Fields           map[string]any `json:"fields" jsonschema:"Only supplied metadata keys change. Explicit null clears; omitted keys preserve. Money and bank objects merge supplied children; tagIds replaces this document's complete existing-tag ID list."`
	ExpectedVersion  *int64         `json:"expectedVersion,omitempty"`
	ExpectedModified string         `json:"expectedModified,omitempty" jsonschema:"Exact modified timestamp returned by get_document_review. Use it because Fileee can keep version at zero across saves."`
}

type setDocumentArchivedInput struct {
	ID               string `json:"id"`
	Archived         bool   `json:"archived" jsonschema:"true removes the document from Dashboard by setting read=true; false returns it to Dashboard. This does not complete any business action or set reviewed/payed."`
	ExpectedVersion  *int64 `json:"expectedVersion,omitempty"`
	ExpectedModified string `json:"expectedModified,omitempty"`
}

// Mutation responses contain only Fileee-owned IDs/state/timestamps. Full
// values are deliberately fetched separately through get_document_review, so
// response framing cannot fail after an already-persisted change.
type reviewMutationOutput struct {
	ID            string   `json:"id"`
	Version       int64    `json:"version"`
	Modified      string   `json:"modified"`
	Archived      *bool    `json:"archived"`
	InDashboard   bool     `json:"inDashboard"`
	Changed       bool     `json:"changed"`
	ChangedFields []string `json:"changedFields"`
}

func reviewMutationResult(doc *fileee.Document, changed bool, fields []string) (*mcp.CallToolResult, reviewMutationOutput) {
	return &mcp.CallToolResult{}, reviewMutationOutput{ID: doc.ID, Version: doc.Version, Modified: reviewTime(doc.Modified), Archived: doc.Attributes.Read, InDashboard: reviewDocumentInDashboard(doc), Changed: changed, ChangedFields: append([]string{}, fields...)}
}

func validateReviewPatchInput(in patchDocumentMetadataInput) ([]string, error) {
	if err := validateReviewID(in.ID); err != nil {
		return nil, err
	}
	if err := validateReviewExpectations(in.ExpectedVersion, in.ExpectedModified); err != nil {
		return nil, err
	}
	return reviewPatchKeys(in.Fields)
}

func reviewPatchSchema(ctx context.Context, refs reviewReferences, cur *fileee.Document, fields map[string]any) (map[string]fileee.SchemaField, error) {
	typeID := cur.Attributes.DocumentTypeID
	if value, ok := fields["documentTypeId"]; ok {
		if value == nil {
			typeID = ""
		} else {
			text, ok := value.(string)
			if !ok || validateReviewID(text) != nil {
				return nil, fmt.Errorf("documentTypeId must be an existing document-type ID or null")
			}
			typeID = text
		}
	}
	schema := make(map[string]fileee.SchemaField)
	if typeID == "" {
		return schema, nil
	}
	if validateReviewID(typeID) != nil || refs.types == nil {
		return nil, fmt.Errorf("cannot validate the current document type")
	}
	typ, err := refs.types.Get(ctx, typeID)
	if err != nil {
		return nil, fmt.Errorf("document type lookup: %w", err)
	}
	if typ == nil || typ.ID != typeID || typ.Deleted {
		return nil, fmt.Errorf("documentTypeId does not resolve to an existing document type")
	}
	if typ.DocumentTypeScheme == "" {
		return schema, nil
	}
	if validateReviewID(typ.DocumentTypeScheme) != nil || refs.schemes == nil {
		return nil, fmt.Errorf("cannot validate the document-type scheme")
	}
	definition, err := refs.schemes.Get(ctx, typ.DocumentTypeScheme)
	if err != nil {
		return nil, fmt.Errorf("document-type scheme lookup: %w", err)
	}
	if definition == nil || definition.ID != typ.DocumentTypeScheme || definition.Deleted {
		return nil, fmt.Errorf("document-type scheme is unavailable")
	}
	for _, field := range definition.Fields() {
		schema[field.Key] = field
	}
	return schema, nil
}

func reviewSchemaPermissions(spec reviewFieldSpec, field fileee.SchemaField) reviewFieldSpec {
	spec.readOnly = field.ReadOnly || field.ServerOnly
	if len(spec.children) != 0 {
		children := make(map[string]reviewFieldSpec, len(spec.children))
		for key, child := range spec.children {
			children[key] = child
		}
		for _, child := range field.ComposingTypes {
			if childSpec, ok := children[child.Key]; ok {
				children[child.Key] = reviewSchemaPermissions(childSpec, child)
			}
		}
		spec.children = children
	}
	return spec
}

func validateReviewReference(ctx context.Context, refs reviewReferences, key string, value any) error {
	if value == nil {
		return nil
	}
	switch key {
	case "senderId", "receiverId":
		id, ok := value.(string)
		if !ok || validateReviewID(id) != nil || refs.contacts == nil {
			return fmt.Errorf("senderId/receiverId must be existing contact IDs or null")
		}
		contact, err := refs.contacts.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("contact lookup: %w", err)
		}
		if contact == nil || contact.ID != id || contact.Deleted {
			return fmt.Errorf("contact ID does not resolve to an existing contact")
		}
	case "tagIds":
		ids, err := reviewStringList(value)
		if err != nil {
			return err
		}
		seen := make(map[string]bool, len(ids))
		for _, id := range ids {
			if validateReviewID(id) != nil || seen[id] || refs.tags == nil {
				return fmt.Errorf("tagIds must be unique existing tag IDs")
			}
			seen[id] = true
			tag, err := refs.tags.Get(ctx, id)
			if err != nil {
				return fmt.Errorf("tag lookup: %w", err)
			}
			if tag == nil || tag.ID != id || tag.Deleted {
				return fmt.Errorf("tag ID does not resolve to an existing tag")
			}
		}
	}
	return nil
}

func buildReviewPatch(ctx context.Context, refs reviewReferences, cur *fileee.Document, fields map[string]any, keys []string) (map[string]json.RawMessage, error) {
	current, err := cur.Attributes.AttributeFields()
	if err != nil {
		return nil, err
	}
	schema, err := reviewPatchSchema(ctx, refs, cur, fields)
	if err != nil {
		return nil, err
	}
	patch := make(map[string]json.RawMessage, len(keys))
	for _, key := range keys {
		if key == "tagIds" {
			for _, action := range cur.ForbiddenActions {
				if action == fileee.ActionEditTags {
					return nil, fmt.Errorf("Fileee forbids changing this document's tags")
				}
			}
		}
		spec, known := reviewMetadataFields[key]
		definition, inSchema := schema[key]
		if !known {
			if !inSchema || len(current[key]) == 0 {
				return nil, fmt.Errorf("unsupported metadata field; custom fields must already exist in the current document and its selected type scheme")
			}
			spec, err = inferReviewField(current[key])
			if err != nil {
				return nil, err
			}
		}
		if inSchema {
			spec = reviewSchemaPermissions(spec, definition)
		}
		if err := validateReviewReference(ctx, refs, key, fields[key]); err != nil {
			return nil, err
		}
		patch[key], err = buildReviewAttribute(current[key], fields[key], spec)
		if err != nil {
			return nil, fmt.Errorf("metadata field %s: %w", key, err)
		}
	}
	return patch, nil
}

func reviewStateJSON(doc *fileee.Document) ([]byte, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	// Normalize object ordering only; never expose this full current DTO,
	// which may contain share tokens or third-party text, in an MCP result.
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func reloadUnchangedReviewDocument(ctx context.Context, service reviewDocumentWriteService, before *fileee.Document) (*fileee.Document, error) {
	fresh, err := service.Get(ctx, before.ID)
	if err != nil {
		return nil, err
	}
	if err := checkReviewCurrent(fresh, before.ID, nil, ""); err != nil {
		return nil, err
	}
	oldState, err := reviewStateJSON(before)
	if err != nil {
		return nil, err
	}
	newState, err := reviewStateJSON(fresh)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(oldState, newState) {
		return nil, fmt.Errorf("document changed during validation; reload get_document_review before saving")
	}
	return fresh, nil
}

func patchDocumentMetadataFromService(ctx context.Context, service reviewDocumentWriteService, refs reviewReferences, in patchDocumentMetadataInput) (*mcp.CallToolResult, reviewMutationOutput, error) {
	keys, err := validateReviewPatchInput(in)
	if err != nil {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolPatchDocumentMetadata, err)
	}
	unlock := lockReviewWrite(ctx, in.ID)
	defer unlock()
	cur, err := service.Get(ctx, in.ID)
	if err == nil {
		err = checkReviewCurrent(cur, in.ID, in.ExpectedVersion, in.ExpectedModified)
	}
	if err != nil {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolPatchDocumentMetadata, err)
	}
	patch, err := buildReviewPatch(ctx, refs, cur, in.Fields, keys)
	if err != nil {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolPatchDocumentMetadata, err)
	}
	fresh, err := reloadUnchangedReviewDocument(ctx, service, cur)
	if err == nil {
		err = fresh.Attributes.PatchAttributeFields(patch)
	}
	if err != nil {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolPatchDocumentMetadata, err)
	}
	oldState, err := reviewStateJSON(cur)
	if err != nil {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolPatchDocumentMetadata, err)
	}
	newState, err := reviewStateJSON(fresh)
	if err != nil {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolPatchDocumentMetadata, err)
	}
	if bytes.Equal(oldState, newState) {
		result, out := reviewMutationResult(fresh, false, nil)
		return result, out, nil
	}
	updated, err := service.Update(ctx, fresh)
	if err != nil {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolPatchDocumentMetadata, err)
	}
	if updated == nil || updated.ID != in.ID {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: save returned no matching document; reload before any retry", ToolPatchDocumentMetadata)
	}
	result, out := reviewMutationResult(updated, true, keys)
	return result, out, nil
}

func setDocumentArchivedFromService(ctx context.Context, service reviewDocumentWriteService, in setDocumentArchivedInput) (*mcp.CallToolResult, reviewMutationOutput, error) {
	if err := validateReviewID(in.ID); err != nil {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolSetDocumentArchived, err)
	}
	if err := validateReviewExpectations(in.ExpectedVersion, in.ExpectedModified); err != nil {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolSetDocumentArchived, err)
	}
	unlock := lockReviewWrite(ctx, in.ID)
	defer unlock()
	cur, err := service.Get(ctx, in.ID)
	if err == nil {
		err = checkReviewCurrent(cur, in.ID, in.ExpectedVersion, in.ExpectedModified)
	}
	if err != nil {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolSetDocumentArchived, err)
	}
	if cur.Attributes.Read != nil && *cur.Attributes.Read == in.Archived {
		result, out := reviewMutationResult(cur, false, nil)
		return result, out, nil
	}
	fresh, err := reloadUnchangedReviewDocument(ctx, service, cur)
	if err != nil {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolSetDocumentArchived, err)
	}
	read, err := buildReviewAttribute(nil, in.Archived, reviewFieldSpec{kind: "BOOLEAN"})
	if err == nil {
		err = fresh.Attributes.PatchAttributeFields(map[string]json.RawMessage{"read": read})
	}
	if err != nil {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolSetDocumentArchived, err)
	}
	updated, err := service.Update(ctx, fresh)
	if err != nil {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolSetDocumentArchived, err)
	}
	if updated == nil || updated.ID != in.ID || updated.Attributes.Read == nil || *updated.Attributes.Read != in.Archived {
		return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: save response did not confirm requested archive state; reload before any retry", ToolSetDocumentArchived)
	}
	result, out := reviewMutationResult(updated, true, []string{"read"})
	return result, out, nil
}

func patchDocumentMetadataHandler(p *clientpool.Pool, logger *slog.Logger) mcp.ToolHandlerFor[patchDocumentMetadataInput, reviewMutationOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in patchDocumentMetadataInput) (*mcp.CallToolResult, reviewMutationOutput, error) {
		start := time.Now()
		logToolStart(ctx, logger, ToolPatchDocumentMetadata)
		if _, err := validateReviewPatchInput(in); err != nil {
			logToolEnd(ctx, logger, ToolPatchDocumentMetadata, start, "", 0, err)
			return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolPatchDocumentMetadata, err)
		}
		client, err := clientFor(ctx, p)
		if err != nil {
			logToolEnd(ctx, logger, ToolPatchDocumentMetadata, start, "", 0, err)
			return nil, reviewMutationOutput{}, err
		}
		result, out, err := patchDocumentMetadataFromService(ctx, client.Documents, reviewReferencesFor(client), in)
		count := 0
		if err == nil {
			count = 1
		}
		logToolEnd(ctx, logger, ToolPatchDocumentMetadata, start, updateDocumentEndpoint, count, err)
		return result, out, err
	}
}

func setDocumentArchivedHandler(p *clientpool.Pool, logger *slog.Logger) mcp.ToolHandlerFor[setDocumentArchivedInput, reviewMutationOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in setDocumentArchivedInput) (*mcp.CallToolResult, reviewMutationOutput, error) {
		start := time.Now()
		logToolStart(ctx, logger, ToolSetDocumentArchived)
		err := validateReviewID(in.ID)
		if err == nil {
			err = validateReviewExpectations(in.ExpectedVersion, in.ExpectedModified)
		}
		if err != nil {
			logToolEnd(ctx, logger, ToolSetDocumentArchived, start, "", 0, err)
			return nil, reviewMutationOutput{}, fmt.Errorf("fileee-mcp: tools: %s: %w", ToolSetDocumentArchived, err)
		}
		client, err := clientFor(ctx, p)
		if err != nil {
			logToolEnd(ctx, logger, ToolSetDocumentArchived, start, "", 0, err)
			return nil, reviewMutationOutput{}, err
		}
		result, out, err := setDocumentArchivedFromService(ctx, client.Documents, in)
		count := 0
		if err == nil {
			count = 1
		}
		logToolEnd(ctx, logger, ToolSetDocumentArchived, start, updateDocumentEndpoint, count, err)
		return result, out, err
	}
}

func reviewMutationInputSchema(archive bool) map[string]any {
	properties := map[string]any{
		"id":               map[string]any{"type": "string", "minLength": 1},
		"expectedVersion":  map[string]any{"type": "integer", "minimum": 0},
		"expectedModified": map[string]any{"type": "string", "format": "date-time", "description": "Current precise modified timestamp from get_document_review; rejects stale state even when version stays zero."},
	}
	required := []string{"id", "expectedModified"}
	if archive {
		properties["archived"] = map[string]any{"type": "boolean"}
		required = append(required, "archived")
	} else {
		fields := make(map[string]any, len(reviewMetadataFields))
		for key, spec := range reviewMetadataFields {
			fields[key] = reviewFieldInputSchema(spec)
		}
		properties["fields"] = map[string]any{"type": "object", "minProperties": 1, "maxProperties": 64, "properties": fields, "additionalProperties": true,
			"description": "Omitted keys preserve; null clears. Group objects merge supplied children. tagIds is the complete desired existing-tag ID list; empty array clears tags. Existing scheme-approved custom fields with supported wrapper kinds are also accepted."}
		required = append(required, "fields")
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func reviewFieldInputSchema(spec reviewFieldSpec) map[string]any {
	switch spec.kind {
	case "COMPOSED":
		children := make(map[string]any, len(spec.children))
		for key, child := range spec.children {
			children[key] = reviewFieldInputSchema(child)
		}
		return map[string]any{"type": []string{"object", "null"}, "properties": children, "additionalProperties": false, "minProperties": 1}
	case "BOOLEAN":
		return map[string]any{"type": []string{"boolean", "null"}}
	case "DOUBLE":
		return map[string]any{"type": []string{"number", "null"}}
	case "INTEGER":
		return map[string]any{"type": []string{"integer", "null"}}
	case "LIST":
		return map[string]any{"type": []string{"array", "null"}, "items": map[string]any{"type": "string"}, "maxItems": 100}
	case "DATE":
		return map[string]any{"type": []string{"string", "null"}, "format": "date", "description": "Calendar date YYYY-MM-DD or null."}
	default:
		return map[string]any{"type": []string{"string", "null"}, "maxLength": 16384}
	}
}

func registerReviewWriteTools(s *mcp.Server, p *clientpool.Pool, logger *slog.Logger) {
	mcp.AddTool(s, &mcp.Tool{
		Name: ToolPatchDocumentMetadata,
		Description: "Patch one existing document's content metadata after reviewing get_document_review. fields uses bare values: strings/IDs, YYYY-MM-DD dates, booleans, money {value,currency}, bankAccount1 subfields and a complete tagIds array. " +
			"Omitted keys preserve; null clears; group objects merge supplied children. Reads full current state twice and preserves all untouched JSON. " +
			"Validates existing contact/tag/type IDs and edit/schema permissions; contact assignment only changes the document and never creates, renames or merges global contacts. " +
			"Requires expectedModified from the reviewed document and optionally accepts expectedVersion to reject stale reviews. Returns state/timestamps/changedFields; re-read full metadata to verify. " +
			"Cannot change read/reviewed/secured/processing or sharing flags; use set_document_archived separately.",
		InputSchema: reviewMutationInputSchema(false),
		Annotations: &mcp.ToolAnnotations{Title: "Patch document review metadata", ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: true},
	}, patchDocumentMetadataHandler(p, logger))
	mcp.AddTool(s, &mcp.Tool{
		Name: ToolSetDocumentArchived,
		Description: "Archive or unarchive one document by setting only attributes.data.read.value (source USER), using a full fresh document save. " +
			"archived=true removes it from Dashboard; false returns it. Reads current state twice, checks edit permissions and rejects stale expectedModified/expectedVersion. " +
			"An already matching state is a no-op. Returns archive/Dashboard state and precise timestamps for verification. " +
			"Archiving changes the filing state. It leaves payment, reminders and the separate reviewed flag untouched.",
		InputSchema: reviewMutationInputSchema(true),
		Annotations: &mcp.ToolAnnotations{Title: "Set document archive state", ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: true},
	}, setDocumentArchivedHandler(p, logger))
}
