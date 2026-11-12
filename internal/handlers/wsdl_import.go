package handlers

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ----------------------------------------------------------------------
// WSDL 1.1 structures (encoding/xml, stdlib only)
//
// We match the WSDL structural elements (definitions, types, message,
// portType, binding, service, port, operation, input) by *local name*
// only — Go's encoding/xml ignores the namespace when a struct tag has
// no namespace prefix, so this works regardless of whether the document
// uses `wsdl:`, `w:`, or no prefix at all.
//
// The one place the namespace matters is the SOAP extensibility elements
// (soap:address / soap:binding / soap:operation). Both SOAP 1.1
// (http://schemas.xmlsoap.org/wsdl/soap/) and SOAP 1.2
// (http://schemas.xmlsoap.org/wsdl/soap12/) reuse the local names
// "address", "binding", and "operation", so we capture the resolved
// xml.Name (which carries the namespace URI) and read the version off it.
// ----------------------------------------------------------------------

type wsdlDefinitions struct {
	XMLName         xml.Name       `xml:"definitions"`
	TargetNamespace string         `xml:"targetNamespace,attr"`
	Types           []wsdlTypes    `xml:"types"`
	Messages        []wsdlMessage  `xml:"message"`
	PortTypes       []wsdlPortType `xml:"portType"`
	Bindings        []wsdlBinding  `xml:"binding"`
	Services        []wsdlService  `xml:"service"`
}

type wsdlTypes struct {
	Schemas []xsdSchema `xml:"schema"`
}

type xsdSchema struct {
	TargetNamespace string           `xml:"targetNamespace,attr"`
	Elements        []xsdElement     `xml:"element"`
	ComplexTypes    []xsdNamedComplex `xml:"complexType"`
}

type xsdElement struct {
	Name        string          `xml:"name,attr"`
	Ref         string          `xml:"ref,attr"`
	Type        string          `xml:"type,attr"`
	ComplexType *xsdComplexType `xml:"complexType"`
}

// xsdNamedComplex is a top-level <complexType name="…"> that an element
// can reference by `type`. Resolving one level through it keeps the
// classic "element points at a named type" WSDLs working without straying
// into deep/nested expansion (explicitly out of scope for v1).
type xsdNamedComplex struct {
	Name        string `xml:"name,attr"`
	xsdComplexType
}

type xsdComplexType struct {
	Sequence *xsdGroup `xml:"sequence"`
	All      *xsdGroup `xml:"all"`
	Choice   *xsdGroup `xml:"choice"`
}

type xsdGroup struct {
	Elements []xsdElement `xml:"element"`
}

type wsdlMessage struct {
	Name  string     `xml:"name,attr"`
	Parts []wsdlPart `xml:"part"`
}

type wsdlPart struct {
	Name    string `xml:"name,attr"`
	Element string `xml:"element,attr"` // document/literal — QName of an <xsd:element>
	Type    string `xml:"type,attr"`    // rpc — QName of a type
}

type wsdlPortType struct {
	Name       string           `xml:"name,attr"`
	Operations []wsdlPortTypeOp `xml:"operation"`
}

type wsdlPortTypeOp struct {
	Name  string        `xml:"name,attr"`
	Input wsdlOpMessage `xml:"input"`
}

type wsdlOpMessage struct {
	Message string `xml:"message,attr"`
	Name    string `xml:"name,attr"`
}

type wsdlBinding struct {
	Name        string          `xml:"name,attr"`
	Type        string          `xml:"type,attr"` // QName of the portType this binding implements
	SoapBinding []xmlNamed      `xml:"binding"`   // soap:binding / soap12:binding (local name "binding")
	Operations  []wsdlBindingOp `xml:"operation"`
}

type wsdlBindingOp struct {
	Name          string              `xml:"name,attr"`
	SoapOperation []wsdlSoapOperation `xml:"operation"` // soap:operation / soap12:operation
}

type wsdlSoapOperation struct {
	XMLName    xml.Name
	SoapAction string `xml:"soapAction,attr"`
}

type wsdlService struct {
	Name  string     `xml:"name,attr"`
	Ports []wsdlPort `xml:"port"`
}

type wsdlPort struct {
	Name      string            `xml:"name,attr"`
	Binding   string            `xml:"binding,attr"`
	Addresses []wsdlSoapAddress `xml:"address"` // soap:address / soap12:address (and possibly http:address)
}

type wsdlSoapAddress struct {
	XMLName  xml.Name
	Location string `xml:"location,attr"`
}

// xmlNamed captures only the resolved element name (with namespace URI).
type xmlNamed struct {
	XMLName xml.Name
}

const (
	soapNS11  = "http://schemas.xmlsoap.org/wsdl/soap/"
	soapNS12  = "http://schemas.xmlsoap.org/wsdl/soap12/"
	envNS11   = "http://schemas.xmlsoap.org/soap/envelope/"
	envNS12   = "http://www.w3.org/2003/05/soap-envelope"
	ctSoap11  = "text/xml; charset=utf-8"
	ctSoap12  = "application/soap+xml; charset=utf-8"
)

// ImportWSDL handles POST /api/import/wsdl.
//
// Input shapes mirror ImportSwagger:
//
//  1. multipart/form-data file upload    — drag-and-drop a .wsdl / .xml
//  2. multipart form `url` field         — server fetches the URL
//  3. raw WSDL text body                 — with optional ?url= / ?sourceUrl=
//
// Optional queries: ?workspaceId=… (same resolution as ImportSwagger) and
// ?name=… (fallback collection name when the WSDL has no <service> name).
func (h *ImportHandler) ImportWSDL(c *gin.Context) {
	userID := c.GetString("userID")

	var specBytes []byte
	var err error
	var urlField string

	contentType := c.ContentType()
	if strings.Contains(contentType, "multipart/form-data") {
		file, _, fileErr := c.Request.FormFile("file")
		urlField = c.PostForm("url")
		switch {
		case fileErr == nil && file != nil:
			defer file.Close()
			specBytes, err = io.ReadAll(file)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read uploaded file"})
				return
			}
		case urlField != "":
			// Reuse the spec resolver's fetch — same polite UA, 15s
			// timeout, HTTP-status check, and 16 MB read cap.
			body, _, fetchErr := NewSpecResolverHandler().fetch(urlField)
			if fetchErr != nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("Failed to fetch WSDL: %v", fetchErr)})
				return
			}
			specBytes = body
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "Provide either a file upload or a URL"})
			return
		}
	} else {
		// Raw WSDL text body. Optional ?url= (preferred) or ?sourceUrl=
		// records where the document came from so it can round-trip into
		// each request's wsdlUrl.
		urlField = c.Query("url")
		if urlField == "" {
			urlField = c.Query("sourceUrl")
		}
		specBytes, err = io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
			return
		}
		// A bare `url` in the query with an empty body → fetch it.
		if len(strings.TrimSpace(string(specBytes))) == 0 && urlField != "" {
			body, _, fetchErr := NewSpecResolverHandler().fetch(urlField)
			if fetchErr != nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("Failed to fetch WSDL: %v", fetchErr)})
				return
			}
			specBytes = body
		}
		if len(specBytes) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Empty request body — send the WSDL XML, a multipart file, or a `url`"})
			return
		}
	}

	var def wsdlDefinitions
	if err := xml.Unmarshal(specBytes, &def); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Failed to parse WSDL: %v", err)})
		return
	}
	if def.XMLName.Local != "definitions" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Not a WSDL 1.1 document (missing <definitions> root)"})
		return
	}

	// Collection name: WSDL service name > provided ?name= > default.
	collName := firstServiceName(def.Services)
	if collName == "" {
		collName = strings.TrimSpace(c.Query("name"))
	}
	if collName == "" {
		collName = "Imported WSDL"
	}

	// Resolve workspace exactly like ImportSwagger so a WSDL import lands
	// in the workspace the FE currently shows.
	workspaceID := c.Query("workspaceId")
	if workspaceID == "__none__" {
		workspaceID = ""
	} else if workspaceID == "" {
		var ws models.Workspace
		if err := h.db.
			Joins("JOIN team_members ON team_members.workspace_id = workspaces.id").
			Where("team_members.user_id = ?", userID).
			Order("workspaces.created_at ASC").
			First(&ws).Error; err == nil {
			workspaceID = ws.ID
		}
	}

	now := time.Now()
	collectionID := uuid.New().String()
	collection := models.Collection{
		ID:          collectionID,
		UserID:      userID,
		WorkspaceID: workspaceID,
		Name:        collName,
		Type:        "collection",
		SourceURL:   urlField,
		SourceType:  "wsdl",
		LastSynced:  &now,
	}
	if err := h.db.Create(&collection).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create collection"})
		return
	}

	count := buildRequestsFromWSDL(h.db, &def, collectionID, urlField)

	h.db.Preload("Children").First(&collection, "id = ?", collectionID)
	var requests []models.SavedRequest
	h.db.Where("collection_id = ?", collectionID).Order("sort_order ASC").Find(&requests)

	notifyCollectionMutated(h.db, c, collectionID, realtime.EventCollectionCreated)

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"collection": collection,
			"requests":   requests,
			"count":      count,
		},
	})
}

// soapBodyBlob is the exact shape stored in SavedRequest.Body for SOAP
// requests. Field order here defines the emitted JSON key order and must
// match the FE's RequestBody (fe/src/types/index.ts).
type soapBodyBlob struct {
	Type          string `json:"type"`
	Content       string `json:"content"`
	SoapVersion   string `json:"soapVersion"`
	SoapAction    string `json:"soapAction"`
	SoapOperation string `json:"soapOperation"`
	WsdlURL       string `json:"wsdlUrl"`
}

// buildRequestsFromWSDL walks every service → port → operation and creates
// one SavedRequest per operation. Returns the number of requests created.
func buildRequestsFromWSDL(db *gorm.DB, def *wsdlDefinitions, collectionID, sourceURL string) int {
	// Index the reusable pieces by local name (references carry prefixes
	// like tns: that we strip so binding→portType→message→element links
	// resolve regardless of the document's prefix choices).
	bindingsByName := map[string]*wsdlBinding{}
	for i := range def.Bindings {
		bindingsByName[localName(def.Bindings[i].Name)] = &def.Bindings[i]
	}
	portTypesByName := map[string]*wsdlPortType{}
	for i := range def.PortTypes {
		portTypesByName[localName(def.PortTypes[i].Name)] = &def.PortTypes[i]
	}
	messagesByName := map[string]*wsdlMessage{}
	for i := range def.Messages {
		messagesByName[localName(def.Messages[i].Name)] = &def.Messages[i]
	}
	elementsByName := map[string]*xsdElement{}
	complexTypesByName := map[string]*xsdComplexType{}
	for _, t := range def.Types {
		for i := range t.Schemas {
			for j := range t.Schemas[i].Elements {
				el := &t.Schemas[i].Elements[j]
				if el.Name != "" {
					elementsByName[el.Name] = el
				}
			}
			for j := range t.Schemas[i].ComplexTypes {
				ct := &t.Schemas[i].ComplexTypes[j]
				if ct.Name != "" {
					complexTypesByName[ct.Name] = &ct.xsdComplexType
				}
			}
		}
	}

	sortOrder := 0
	count := 0

	for si := range def.Services {
		svc := &def.Services[si]
		for pi := range svc.Ports {
			port := &svc.Ports[pi]

			endpoint, addrVersion := portEndpoint(port)
			binding := bindingsByName[localName(port.Binding)]

			// SOAP version: prefer the port's soap:address namespace,
			// then the binding's soap:binding, then a binding operation's
			// soap:operation. Default 1.1.
			version := addrVersion
			if version == "" && binding != nil {
				for _, sb := range binding.SoapBinding {
					if v := soapVersionFromNS(sb.XMLName.Space); v != "" {
						version = v
						break
					}
				}
			}
			if version == "" && binding != nil {
				for _, bop := range binding.Operations {
					for _, so := range bop.SoapOperation {
						if v := soapVersionFromNS(so.XMLName.Space); v != "" {
							version = v
							break
						}
					}
					if version != "" {
						break
					}
				}
			}
			if version == "" {
				version = "1.1"
			}

			// soapAction per binding operation.
			actionByOp := map[string]string{}
			if binding != nil {
				for _, bop := range binding.Operations {
					for _, so := range bop.SoapOperation {
						actionByOp[bop.Name] = so.SoapAction
						break
					}
				}
			}

			// Operation set: prefer the binding's operations (that's what
			// the port actually exposes); fall back to the abstract
			// portType when the binding lists none.
			pt := portTypesByName[localName(binding.typeRef())]
			var opNames []string
			if binding != nil && len(binding.Operations) > 0 {
				for _, bop := range binding.Operations {
					opNames = append(opNames, bop.Name)
				}
			} else if pt != nil {
				for _, op := range pt.Operations {
					opNames = append(opNames, op.Name)
				}
			}

			for _, opName := range opNames {
				action := actionByOp[opName]

				// Resolve the input element name + its direct children.
				topElement, children := resolveOpBody(opName, pt, messagesByName, elementsByName, complexTypesByName)

				envelope := buildSoapEnvelope(def.TargetNamespace, version, topElement, children)

				blob := soapBodyBlob{
					Type:          "soap",
					Content:       envelope,
					SoapVersion:   version,
					SoapAction:    action,
					SoapOperation: opName,
					WsdlURL:       sourceURL,
				}
				bodyJSON, _ := json.Marshal(blob)

				headers := []map[string]interface{}{
					{"key": "Content-Type", "value": contentTypeFor(version, action), "enabled": true},
				}
				if version == "1.1" {
					// SOAP 1.1 carries the action in a separate quoted
					// header; 1.2 folds it into Content-Type (above).
					val := `""`
					if action != "" {
						val = `"` + action + `"`
					}
					headers = append(headers, map[string]interface{}{"key": "SOAPAction", "value": val, "enabled": true})
				}
				headersJSON, _ := json.Marshal(headers)

				req := models.SavedRequest{
					ID:           uuid.New().String(),
					CollectionID: collectionID,
					Name:         opName,
					Method:       "POST",
					URL:          endpoint,
					Headers:      string(headersJSON),
					Params:       "[]",
					Body:         string(bodyJSON),
					Auth:         `{"type":"none"}`,
					SortOrder:    sortOrder,
				}
				db.Create(&req)
				sortOrder++
				count++
			}
		}
	}

	return count
}

// typeRef returns the binding's portType QName, tolerating a nil binding.
func (b *wsdlBinding) typeRef() string {
	if b == nil {
		return ""
	}
	return b.Type
}

// portEndpoint returns the port's endpoint location and the SOAP version
// implied by the address namespace (empty version if the address isn't a
// SOAP address). Prefers a soap:address / soap12:address over any other
// <address> (e.g. an http:address on a non-SOAP port).
func portEndpoint(p *wsdlPort) (location, version string) {
	for _, a := range p.Addresses {
		if v := soapVersionFromNS(a.XMLName.Space); v != "" {
			return a.Location, v
		}
	}
	// No SOAP address — fall back to the first address for the URL.
	if len(p.Addresses) > 0 {
		return p.Addresses[0].Location, ""
	}
	return "", ""
}

// resolveOpBody figures out the top-level body element name and its direct
// (one-level) child element names for an operation. Deep/nested XSD
// expansion is out of scope for v1.
//
//   - document/literal: the input message's part with an `element` attr
//     points at an <xsd:element>; that element name is the body element and
//     its complexType's direct children are the placeholders.
//   - RPC style (parts use `type`, no `element`), or an unresolvable
//     element/message: fall back to a bare operation element (no children).
func resolveOpBody(opName string, pt *wsdlPortType, messages map[string]*wsdlMessage, elements map[string]*xsdElement, complexTypes map[string]*xsdComplexType) (topElement string, children []string) {
	topElement = opName // safe default (also the RPC-style element)

	if pt == nil {
		return topElement, nil
	}
	var op *wsdlPortTypeOp
	for i := range pt.Operations {
		if pt.Operations[i].Name == opName {
			op = &pt.Operations[i]
			break
		}
	}
	if op == nil || op.Input.Message == "" {
		return topElement, nil
	}
	msg := messages[localName(op.Input.Message)]
	if msg == nil {
		return topElement, nil
	}
	// Find the part carrying a document/literal element reference.
	var elemRef string
	for _, part := range msg.Parts {
		if part.Element != "" {
			elemRef = part.Element
			break
		}
	}
	if elemRef == "" {
		// RPC style — bare operation element, no reliable children.
		return topElement, nil
	}
	el := elements[localName(elemRef)]
	if el == nil {
		// Element not found in <types> — emit just that element name.
		return localName(elemRef), nil
	}
	topElement = el.Name
	return topElement, directChildElements(el, complexTypes)
}

// directChildElements returns the names of an element's immediate child
// elements — from an inline complexType, or (one level) from a named
// complexType referenced via the element's `type`.
func directChildElements(el *xsdElement, complexTypes map[string]*xsdComplexType) []string {
	ct := el.ComplexType
	if ct == nil && el.Type != "" {
		ct = complexTypes[localName(el.Type)]
	}
	if ct == nil {
		return nil
	}
	var names []string
	for _, g := range []*xsdGroup{ct.Sequence, ct.All, ct.Choice} {
		if g == nil {
			continue
		}
		for _, child := range g.Elements {
			switch {
			case child.Name != "":
				names = append(names, child.Name)
			case child.Ref != "":
				names = append(names, localName(child.Ref))
			}
		}
	}
	return names
}

// buildSoapEnvelope renders the shallow SOAP envelope skeleton. When the
// target namespace is empty (no prefix to bind) the body element is emitted
// unqualified. When there are no children the operation element carries a
// TODO comment placeholder.
func buildSoapEnvelope(targetNS, version, topElement string, children []string) string {
	envNS := envNS11
	if version == "1.2" {
		envNS = envNS12
	}

	prefix := "tns:"
	var b strings.Builder
	b.WriteString(`<soapenv:Envelope xmlns:soapenv="`)
	b.WriteString(envNS)
	b.WriteString(`"`)
	if targetNS != "" {
		b.WriteString(` xmlns:tns="`)
		b.WriteString(targetNS)
		b.WriteString(`"`)
	} else {
		prefix = "" // nothing to bind tns to — emit unqualified
	}
	b.WriteString(">\n")
	b.WriteString("  <soapenv:Header/>\n")
	b.WriteString("  <soapenv:Body>\n")

	if len(children) == 0 {
		b.WriteString("    <")
		b.WriteString(prefix + topElement)
		b.WriteString(">\n")
		b.WriteString("      <!-- TODO: fill request body -->\n")
		b.WriteString("    </")
		b.WriteString(prefix + topElement)
		b.WriteString(">\n")
	} else {
		b.WriteString("    <")
		b.WriteString(prefix + topElement)
		b.WriteString(">\n")
		for _, ch := range children {
			b.WriteString("      <")
			b.WriteString(prefix + ch)
			b.WriteString(">?</")
			b.WriteString(prefix + ch)
			b.WriteString(">\n")
		}
		b.WriteString("    </")
		b.WriteString(prefix + topElement)
		b.WriteString(">\n")
	}

	b.WriteString("  </soapenv:Body>\n")
	b.WriteString("</soapenv:Envelope>")
	return b.String()
}

func contentTypeFor(version, action string) string {
	if version == "1.2" {
		if action != "" {
			return ctSoap12 + `; action="` + action + `"`
		}
		return ctSoap12
	}
	return ctSoap11
}

func firstServiceName(services []wsdlService) string {
	for _, s := range services {
		if strings.TrimSpace(s.Name) != "" {
			return s.Name
		}
	}
	return ""
}

// soapVersionFromNS maps a WSDL SOAP binding namespace to "1.1"/"1.2".
func soapVersionFromNS(ns string) string {
	switch ns {
	case soapNS12:
		return "1.2"
	case soapNS11:
		return "1.1"
	default:
		return ""
	}
}

// localName strips a QName prefix ("tns:Add" → "Add").
func localName(qname string) string {
	if i := strings.LastIndex(qname, ":"); i >= 0 {
		return qname[i+1:]
	}
	return qname
}
