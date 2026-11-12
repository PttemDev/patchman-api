package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type DocsHandler struct{}

func NewDocsHandler() *DocsHandler {
	return &DocsHandler{}
}

// ScalarUI serves the Scalar API documentation page
func (h *DocsHandler) ScalarUI(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, scalarHTML)
}

// OpenAPISpec serves the OpenAPI JSON specification
func (h *DocsHandler) OpenAPISpec(c *gin.Context) {
	c.Header("Content-Type", "application/json")
	c.String(http.StatusOK, openAPISpecJSON)
}

const scalarHTML = `<!DOCTYPE html>
<html>
<head>
  <title>PatchMan API</title>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
</head>
<body>
  <script id="api-reference" data-url="/api/docs/openapi.json"></script>
  <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
</body>
</html>`

const openAPISpecJSON = `{
  "openapi": "3.0.3",
  "info": {
    "title": "PatchMan API",
    "description": "Self-hosted API development & testing platform. Postman alternative with collaboration, mock servers, test management, and more.",
    "version": "1.0.0"
  },
  "servers": [
    { "url": "/api", "description": "Current server" }
  ],
  "components": {
    "securitySchemes": {
      "bearerAuth": {
        "type": "http",
        "scheme": "bearer",
        "bearerFormat": "JWT"
      }
    },
    "schemas": {
      "Error": {
        "type": "object",
        "properties": { "error": { "type": "string" } }
      },
      "Collection": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "userId": { "type": "string" },
          "name": { "type": "string" },
          "description": { "type": "string" },
          "parentId": { "type": "string", "nullable": true },
          "type": { "type": "string", "enum": ["collection", "folder"] },
          "sortOrder": { "type": "integer" },
          "baseUrl": { "type": "string", "description": "Base URL prepended to relative request paths" },
          "sourceUrl": { "type": "string", "description": "URL of original Swagger/OpenAPI source for sync" },
          "sourceType": { "type": "string", "description": "Source type (swagger, postman)" },
          "lastSynced": { "type": "string", "format": "date-time", "nullable": true },
          "children": { "type": "array", "items": { "$ref": "#/components/schemas/Collection" } },
          "requests": { "type": "array", "items": { "$ref": "#/components/schemas/SavedRequest" } },
          "shared": { "type": "boolean" },
          "ownerName": { "type": "string" }
        }
      },
      "SavedRequest": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "collectionId": { "type": "string" },
          "name": { "type": "string" },
          "method": { "type": "string" },
          "url": { "type": "string" },
          "headers": { "type": "string", "description": "JSON string of key-value array" },
          "params": { "type": "string", "description": "JSON string of key-value array" },
          "body": { "type": "string", "description": "JSON string {type, content}" },
          "auth": { "type": "string", "description": "JSON string {type, ...}" },
          "preRequestScript": { "type": "string" },
          "testScript": { "type": "string" },
          "sortOrder": { "type": "integer" }
        }
      },
      "Environment": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "name": { "type": "string" },
          "isActive": { "type": "boolean" },
          "variables": { "type": "string", "description": "JSON array of {key, value, enabled, secret}" }
        }
      },
      "MockServer": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "name": { "type": "string" },
          "prefix": { "type": "string" },
          "isActive": { "type": "boolean" }
        }
      },
      "MockRoute": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "mockServerId": { "type": "string" },
          "method": { "type": "string" },
          "path": { "type": "string" },
          "statusCode": { "type": "integer" },
          "responseHeaders": { "type": "string" },
          "responseBody": { "type": "string" },
          "delay": { "type": "integer" },
          "isActive": { "type": "boolean" }
        }
      },
      "TestCase": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "title": { "type": "string" },
          "description": { "type": "string" },
          "priority": { "type": "string" },
          "type": { "type": "string" },
          "tags": { "type": "string" },
          "steps": { "type": "string" },
          "status": { "type": "string" }
        }
      },
      "TestPlan": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "name": { "type": "string" },
          "description": { "type": "string" },
          "status": { "type": "string" }
        }
      },
      "TestRun": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "testPlanId": { "type": "string" },
          "status": { "type": "string" },
          "results": { "type": "string" }
        }
      },
      "Workspace": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "name": { "type": "string" },
          "description": { "type": "string" },
          "ownerId": { "type": "string" }
        }
      },
      "Comment": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "content": { "type": "string" },
          "entityType": { "type": "string" },
          "entityId": { "type": "string" },
          "userId": { "type": "string" }
        }
      },
      "SharedStep": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "title": { "type": "string" },
          "steps": { "type": "string" }
        }
      },
      "Defect": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "title": { "type": "string" },
          "description": { "type": "string" },
          "severity": { "type": "string" },
          "status": { "type": "string" }
        }
      },
      "Requirement": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "title": { "type": "string" },
          "description": { "type": "string" },
          "priority": { "type": "string" },
          "status": { "type": "string" }
        }
      },
      "LoadTest": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "name": { "type": "string" },
          "config": { "type": "object" },
          "status": { "type": "string" },
          "results": { "type": "object" }
        }
      },
      "HistoryItem": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "method": { "type": "string" },
          "url": { "type": "string" },
          "statusCode": { "type": "integer" },
          "duration": { "type": "integer" },
          "createdAt": { "type": "string", "format": "date-time" }
        }
      }
    }
  },
  "security": [{ "bearerAuth": [] }],
  "paths": {
    "/auth/register": {
      "post": {
        "tags": ["Auth"],
        "summary": "Register a new user",
        "security": [],
        "requestBody": {
          "required": true,
          "content": { "application/json": { "schema": { "type": "object", "required": ["name", "email", "password"], "properties": { "name": { "type": "string" }, "email": { "type": "string", "format": "email" }, "password": { "type": "string", "minLength": 6 } } } } }
        },
        "responses": { "201": { "description": "User created with JWT token" }, "400": { "description": "Validation error" }, "409": { "description": "Email already exists" } }
      }
    },
    "/auth/login": {
      "post": {
        "tags": ["Auth"],
        "summary": "Login and get JWT token",
        "security": [],
        "requestBody": {
          "required": true,
          "content": { "application/json": { "schema": { "type": "object", "required": ["email", "password"], "properties": { "email": { "type": "string" }, "password": { "type": "string" } } } } }
        },
        "responses": { "200": { "description": "JWT token + user info" }, "401": { "description": "Invalid credentials" } }
      }
    },
    "/auth/me": {
      "get": {
        "tags": ["Auth"],
        "summary": "Get current user info",
        "responses": { "200": { "description": "User object" } }
      }
    },
    "/users/search": {
      "get": {
        "tags": ["Users"],
        "summary": "Search users by name or email",
        "parameters": [{ "name": "q", "in": "query", "required": true, "schema": { "type": "string" }, "description": "Search query (matches name or email)" }],
        "responses": { "200": { "description": "Array of matching users (id, name, email)", "content": { "application/json": { "schema": { "type": "array", "items": { "type": "object", "properties": { "id": { "type": "string" }, "name": { "type": "string" }, "email": { "type": "string" } } } } } } } }
      }
    },
    "/collections": {
      "get": {
        "tags": ["Collections"],
        "summary": "List all collections (owned + shared)",
        "responses": { "200": { "description": "Array of collections with children and requests" } }
      },
      "post": {
        "tags": ["Collections"],
        "summary": "Create a collection",
        "requestBody": {
          "required": true,
          "content": { "application/json": { "schema": { "type": "object", "required": ["name"], "properties": { "name": { "type": "string" }, "description": { "type": "string" }, "parentId": { "type": "string" }, "type": { "type": "string", "enum": ["collection", "folder"] } } } } }
        },
        "responses": { "201": { "description": "Created collection" } }
      }
    },
    "/collections/{id}": {
      "put": {
        "tags": ["Collections"],
        "summary": "Update a collection",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "requestBody": { "content": { "application/json": { "schema": { "type": "object", "properties": { "name": { "type": "string" }, "description": { "type": "string" }, "baseUrl": { "type": "string", "description": "Base URL prepended to relative request paths" }, "sortOrder": { "type": "integer" } } } } } },
        "responses": { "200": { "description": "Updated collection" } }
      },
      "delete": {
        "tags": ["Collections"],
        "summary": "Delete a collection and all its contents",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Deleted" } }
      }
    },
    "/collections/{id}/requests": {
      "get": {
        "tags": ["Collections"],
        "summary": "List requests in a collection",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Array of saved requests" } }
      }
    },
    "/collections/{id}/export": {
      "get": {
        "tags": ["Collections"],
        "summary": "Export collection as JSON",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Collection + requests JSON" } }
      }
    },
    "/collections/import": {
      "post": {
        "tags": ["Collections"],
        "summary": "Import collection from JSON",
        "responses": { "201": { "description": "Imported collection" } }
      }
    },
    "/collections/{id}/sync": {
      "post": {
        "tags": ["Collections"],
        "summary": "Sync/update collection from external source",
        "description": "Supports 4 modes: refresh (re-fetch from stored sourceUrl), url (new Swagger URL), json (raw OpenAPI spec), postman (Postman collection JSON).",
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } },
          { "name": "mode", "in": "query", "required": true, "schema": { "type": "string", "enum": ["refresh", "url", "json", "postman"] } }
        ],
        "requestBody": { "content": { "application/json": { "schema": { "type": "object", "properties": { "url": { "type": "string", "description": "Swagger/OpenAPI URL (mode=url)" } } } } } },
        "responses": { "200": { "description": "Sync result with count of synced requests" }, "400": { "description": "Invalid mode or missing parameters" } }
      }
    },
    "/collections/{id}/share": {
      "post": {
        "tags": ["Collaboration"],
        "summary": "Generate share code for a collection",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "201": { "description": "Share token" }, "200": { "description": "Existing share token" } }
      },
      "delete": {
        "tags": ["Collaboration"],
        "summary": "Revoke sharing (removes all collaborators)",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Sharing revoked" } }
      }
    },
    "/collections/join": {
      "post": {
        "tags": ["Collaboration"],
        "summary": "Join a shared collection by code",
        "requestBody": {
          "required": true,
          "content": { "application/json": { "schema": { "type": "object", "required": ["shareToken"], "properties": { "shareToken": { "type": "string" } } } } }
        },
        "responses": { "200": { "description": "Joined successfully" }, "404": { "description": "Invalid share code" }, "409": { "description": "Already a member" } }
      }
    },
    "/collections/{id}/collaborators": {
      "get": {
        "tags": ["Collaboration"],
        "summary": "List collaborators",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Collaborators + share token" } }
      }
    },
    "/collections/{id}/collaborators/{userId}": {
      "delete": {
        "tags": ["Collaboration"],
        "summary": "Remove a collaborator",
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } },
          { "name": "userId", "in": "path", "required": true, "schema": { "type": "string" } }
        ],
        "responses": { "200": { "description": "Removed" } }
      }
    },
    "/requests": {
      "post": {
        "tags": ["Requests"],
        "summary": "Create a saved request",
        "requestBody": {
          "required": true,
          "content": { "application/json": { "schema": { "type": "object", "required": ["collectionId", "name"], "properties": { "collectionId": { "type": "string" }, "name": { "type": "string" }, "method": { "type": "string" }, "url": { "type": "string" }, "headers": { "type": "string" }, "params": { "type": "string" }, "body": { "type": "string" }, "auth": { "type": "string" } } } } }
        },
        "responses": { "201": { "description": "Created request" } }
      }
    },
    "/requests/{id}": {
      "get": {
        "tags": ["Requests"],
        "summary": "Get a saved request",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Request object" } }
      },
      "put": {
        "tags": ["Requests"],
        "summary": "Update a saved request",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Updated request" } }
      },
      "delete": {
        "tags": ["Requests"],
        "summary": "Delete a saved request",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Deleted" } }
      }
    },
    "/proxy/send": {
      "post": {
        "tags": ["Proxy"],
        "summary": "Send HTTP request through backend proxy",
        "requestBody": {
          "required": true,
          "content": { "application/json": { "schema": { "type": "object", "required": ["method", "url"], "properties": { "method": { "type": "string" }, "url": { "type": "string" }, "headers": { "type": "object" }, "body": { "type": "object" }, "params": { "type": "object" } } } } }
        },
        "responses": { "200": { "description": "Proxied response with timing info" } }
      }
    },
    "/cors-proxy": {
      "post": {
        "tags": ["Proxy"],
        "summary": "Lightweight transparent CORS proxy",
        "description": "Forwards the request as-is to the target URL, adding CORS headers to the response.",
        "requestBody": {
          "required": true,
          "content": { "application/json": { "schema": { "type": "object", "required": ["url"], "properties": { "url": { "type": "string" }, "method": { "type": "string" }, "headers": { "type": "object" }, "body": {} } } } }
        },
        "responses": { "200": { "description": "Proxied response" } }
      }
    },
    "/environments": {
      "get": { "tags": ["Environments"], "summary": "List environments", "responses": { "200": { "description": "Array of environments" } } },
      "post": { "tags": ["Environments"], "summary": "Create environment", "responses": { "201": { "description": "Created" } } }
    },
    "/environments/{id}": {
      "put": { "tags": ["Environments"], "summary": "Update environment", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Updated" } } },
      "delete": { "tags": ["Environments"], "summary": "Delete environment", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Deleted" } } }
    },
    "/environments/{id}/activate": {
      "put": { "tags": ["Environments"], "summary": "Set environment as active", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Activated" } } }
    },
    "/history": {
      "get": {
        "tags": ["History"],
        "summary": "List request history",
        "description": "Returns paginated history. Entries older than 30 days are automatically cleaned up.",
        "parameters": [{ "name": "page", "in": "query", "schema": { "type": "integer" } }, { "name": "limit", "in": "query", "schema": { "type": "integer" } }],
        "responses": { "200": { "description": "Paginated history items" } }
      },
      "delete": { "tags": ["History"], "summary": "Clear all history", "responses": { "200": { "description": "Cleared" } } }
    },
    "/history/{id}": {
      "delete": {
        "tags": ["History"],
        "summary": "Delete a single history entry",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Deleted" }, "404": { "description": "Not found" } }
      }
    },
    "/codegen": {
      "post": { "tags": ["Utilities"], "summary": "Generate code snippet from request", "requestBody": { "content": { "application/json": { "schema": { "type": "object", "properties": { "method": { "type": "string" }, "url": { "type": "string" }, "headers": { "type": "object" }, "body": { "type": "string" }, "language": { "type": "string", "enum": ["curl", "python", "javascript", "go", "java", "csharp", "php", "ruby"] } } } } } }, "responses": { "200": { "description": "Generated code" } } }
    },
    "/search": {
      "get": { "tags": ["Utilities"], "summary": "Global search across all resources", "parameters": [{ "name": "q", "in": "query", "required": true, "schema": { "type": "string" } }, { "name": "type", "in": "query", "schema": { "type": "string" } }], "responses": { "200": { "description": "Search results" } } }
    },
    "/script/execute": {
      "post": {
        "tags": ["Utilities"],
        "summary": "Execute a pre-request or test script",
        "requestBody": { "content": { "application/json": { "schema": { "type": "object", "properties": { "script": { "type": "string" }, "context": { "type": "object" } } } } } },
        "responses": { "200": { "description": "Script execution result" } }
      }
    },
    "/mock-servers": {
      "get": { "tags": ["Mock Servers"], "summary": "List mock servers", "responses": { "200": { "description": "Array of mock servers" } } },
      "post": { "tags": ["Mock Servers"], "summary": "Create mock server", "responses": { "201": { "description": "Created" } } }
    },
    "/mock-servers/{id}": {
      "put": { "tags": ["Mock Servers"], "summary": "Update mock server", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Updated" } } },
      "delete": { "tags": ["Mock Servers"], "summary": "Delete mock server", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Deleted" } } }
    },
    "/mock-servers/{id}/routes": {
      "get": { "tags": ["Mock Servers"], "summary": "List routes for a mock server", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Array of routes" } } },
      "post": { "tags": ["Mock Servers"], "summary": "Create route", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "201": { "description": "Created" } } }
    },
    "/mock-routes/{id}": {
      "put": { "tags": ["Mock Servers"], "summary": "Update a mock route", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Updated" } } },
      "delete": { "tags": ["Mock Servers"], "summary": "Delete a mock route", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Deleted" } } }
    },
    "/test-cases": {
      "get": { "tags": ["Test Management"], "summary": "List test cases", "responses": { "200": { "description": "Array of test cases" } } },
      "post": { "tags": ["Test Management"], "summary": "Create test case", "responses": { "201": { "description": "Created" } } }
    },
    "/test-cases/{id}": {
      "get": { "tags": ["Test Management"], "summary": "Get test case details", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Test case" } } },
      "put": { "tags": ["Test Management"], "summary": "Update test case", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Updated" } } },
      "delete": { "tags": ["Test Management"], "summary": "Delete test case", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Deleted" } } }
    },
    "/test-cases/{id}/execute": {
      "post": { "tags": ["Test Management"], "summary": "Execute a test case", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Execution results" } } }
    },
    "/test-cases/{id}/versions": {
      "get": {
        "tags": ["Test Management"],
        "summary": "List test case versions",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Array of versions" } }
      }
    },
    "/test-cases/{id}/versions/{versionId}": {
      "get": {
        "tags": ["Test Management"],
        "summary": "Get a specific test case version",
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } },
          { "name": "versionId", "in": "path", "required": true, "schema": { "type": "string" } }
        ],
        "responses": { "200": { "description": "Version details" } }
      }
    },
    "/test-cases/{id}/versions/{versionId}/restore": {
      "post": {
        "tags": ["Test Management"],
        "summary": "Restore a test case to a previous version",
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } },
          { "name": "versionId", "in": "path", "required": true, "schema": { "type": "string" } }
        ],
        "responses": { "200": { "description": "Restored" } }
      }
    },
    "/test-plans": {
      "get": { "tags": ["Test Management"], "summary": "List test plans", "responses": { "200": { "description": "Array of test plans" } } },
      "post": { "tags": ["Test Management"], "summary": "Create test plan", "responses": { "201": { "description": "Created" } } }
    },
    "/test-plans/{id}": {
      "get": { "tags": ["Test Management"], "summary": "Get test plan details", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Test plan" } } },
      "put": { "tags": ["Test Management"], "summary": "Update test plan", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Updated" } } },
      "delete": { "tags": ["Test Management"], "summary": "Delete test plan", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Deleted" } } }
    },
    "/test-plans/{id}/execute": {
      "post": { "tags": ["Test Management"], "summary": "Execute a test plan", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Execution results" } } }
    },
    "/test-plans/{id}/runs": {
      "get": {
        "tags": ["Test Runs"],
        "summary": "List runs for a test plan",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Array of test runs" } }
      },
      "post": {
        "tags": ["Test Runs"],
        "summary": "Create a test run for a plan",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "201": { "description": "Created test run" } }
      }
    },
    "/test-runs/{id}": {
      "get": {
        "tags": ["Test Runs"],
        "summary": "Get test run details",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Test run" } }
      },
      "put": {
        "tags": ["Test Runs"],
        "summary": "Update test run",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Updated" } }
      }
    },
    "/shared-steps": {
      "get": { "tags": ["Test Management"], "summary": "List shared steps", "responses": { "200": { "description": "Array of shared steps" } } },
      "post": { "tags": ["Test Management"], "summary": "Create shared step", "responses": { "201": { "description": "Created" } } }
    },
    "/shared-steps/{id}": {
      "get": { "tags": ["Test Management"], "summary": "Get shared step", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Shared step" } } },
      "put": { "tags": ["Test Management"], "summary": "Update shared step", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Updated" } } },
      "delete": { "tags": ["Test Management"], "summary": "Delete shared step", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Deleted" } } }
    },
    "/runner/run": {
      "post": { "tags": ["Runner"], "summary": "Run collection requests sequentially", "requestBody": { "content": { "application/json": { "schema": { "type": "object", "required": ["collectionId"], "properties": { "collectionId": { "type": "string" }, "environmentId": { "type": "string" }, "delay": { "type": "integer" }, "iterations": { "type": "integer" } } } } } }, "responses": { "200": { "description": "Run results" } } }
    },
    "/runner/data-driven": {
      "post": { "tags": ["Runner"], "summary": "Run collection with data-driven testing", "responses": { "200": { "description": "Run results with iterations" } } }
    },
    "/load-tests": {
      "get": { "tags": ["Load Testing"], "summary": "List load tests", "responses": { "200": { "description": "Array" } } },
      "post": { "tags": ["Load Testing"], "summary": "Create and start a load test", "responses": { "201": { "description": "Created" } } }
    },
    "/load-tests/{id}": {
      "get": { "tags": ["Load Testing"], "summary": "Get load test details", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Load test" } } },
      "delete": { "tags": ["Load Testing"], "summary": "Delete load test", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Deleted" } } }
    },
    "/load-tests/{id}/live": {
      "get": {
        "tags": ["Load Testing"],
        "summary": "Stream live load test results (SSE)",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "SSE stream of live results" } }
      }
    },
    "/load-tests/{id}/cancel": {
      "post": {
        "tags": ["Load Testing"],
        "summary": "Cancel a running load test",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Cancelled" } }
      }
    },
    "/import/swagger": {
      "post": { "tags": ["Import"], "summary": "Import from Swagger/OpenAPI spec", "description": "Accepts file upload or URL. Parses OpenAPI 3.x and Swagger 2.0 specs. Automatically extracts base URL from servers/host fields.", "requestBody": { "content": { "multipart/form-data": { "schema": { "type": "object", "properties": { "file": { "type": "string", "format": "binary" }, "url": { "type": "string" } } } } } }, "responses": { "201": { "description": "Imported collection with base URL" } } }
    },
    "/import/postman": {
      "post": { "tags": ["Import"], "summary": "Import Postman collection v2.1", "responses": { "201": { "description": "Imported collection" } } }
    },
    "/import/junit": {
      "get": { "tags": ["Import"], "summary": "List JUnit XML imports", "responses": { "200": { "description": "Array of imports" } } },
      "post": { "tags": ["Import"], "summary": "Import JUnit XML test results", "responses": { "201": { "description": "Imported" } } }
    },
    "/import/junit/{id}": {
      "get": { "tags": ["Import"], "summary": "Get JUnit import details", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Import details" } } },
      "delete": { "tags": ["Import"], "summary": "Delete JUnit import", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Deleted" } } }
    },
    "/workspaces": {
      "get": { "tags": ["Workspaces"], "summary": "List workspaces", "responses": { "200": { "description": "Array of workspaces" } } },
      "post": { "tags": ["Workspaces"], "summary": "Create workspace", "responses": { "201": { "description": "Created" } } }
    },
    "/workspaces/{id}": {
      "get": { "tags": ["Workspaces"], "summary": "Get workspace", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Workspace" } } },
      "put": { "tags": ["Workspaces"], "summary": "Update workspace", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Updated" } } },
      "delete": { "tags": ["Workspaces"], "summary": "Delete workspace", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Deleted" } } }
    },
    "/workspaces/{id}/members": {
      "get": { "tags": ["Workspaces"], "summary": "List workspace members", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Array of members" } } },
      "post": {
        "tags": ["Workspaces"],
        "summary": "Invite member to workspace",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "201": { "description": "Invited" } }
      }
    },
    "/workspaces/{id}/members/{memberId}": {
      "put": {
        "tags": ["Workspaces"],
        "summary": "Update member role",
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } },
          { "name": "memberId", "in": "path", "required": true, "schema": { "type": "string" } }
        ],
        "responses": { "200": { "description": "Updated" } }
      },
      "delete": {
        "tags": ["Workspaces"],
        "summary": "Remove member from workspace",
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } },
          { "name": "memberId", "in": "path", "required": true, "schema": { "type": "string" } }
        ],
        "responses": { "200": { "description": "Removed" } }
      }
    },
    "/defects": {
      "get": { "tags": ["Defects"], "summary": "List defects", "responses": { "200": { "description": "Array" } } },
      "post": { "tags": ["Defects"], "summary": "Create defect", "responses": { "201": { "description": "Created" } } }
    },
    "/defects/{id}": {
      "get": { "tags": ["Defects"], "summary": "Get defect", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Defect" } } },
      "put": { "tags": ["Defects"], "summary": "Update defect", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Updated" } } },
      "delete": { "tags": ["Defects"], "summary": "Delete defect", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Deleted" } } }
    },
    "/comments": {
      "get": {
        "tags": ["Comments"],
        "summary": "List comments",
        "parameters": [{ "name": "entityType", "in": "query", "schema": { "type": "string" } }, { "name": "entityId", "in": "query", "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Array of comments" } }
      },
      "post": { "tags": ["Comments"], "summary": "Create comment", "responses": { "201": { "description": "Created" } } }
    },
    "/comments/{id}": {
      "put": { "tags": ["Comments"], "summary": "Update comment", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Updated" } } },
      "delete": { "tags": ["Comments"], "summary": "Delete comment", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Deleted" } } }
    },
    "/audit-logs": {
      "get": {
        "tags": ["Audit"],
        "summary": "List audit logs",
        "responses": { "200": { "description": "Array of audit log entries" } }
      }
    },
    "/activity-feed": {
      "get": {
        "tags": ["Audit"],
        "summary": "List activity feed",
        "responses": { "200": { "description": "Array of activity entries" } }
      }
    },
    "/requirements": {
      "get": { "tags": ["Requirements"], "summary": "List requirements", "responses": { "200": { "description": "Array" } } },
      "post": { "tags": ["Requirements"], "summary": "Create requirement", "responses": { "201": { "description": "Created" } } }
    },
    "/requirements/{id}": {
      "get": { "tags": ["Requirements"], "summary": "Get requirement", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Requirement" } } },
      "put": { "tags": ["Requirements"], "summary": "Update requirement", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Updated" } } },
      "delete": { "tags": ["Requirements"], "summary": "Delete requirement", "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }], "responses": { "200": { "description": "Deleted" } } }
    },
    "/requirements/{id}/test-cases": {
      "post": {
        "tags": ["Requirements"],
        "summary": "Link a test case to a requirement",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Linked" } }
      }
    },
    "/requirements/{id}/test-cases/{testCaseId}": {
      "delete": {
        "tags": ["Requirements"],
        "summary": "Unlink a test case from a requirement",
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } },
          { "name": "testCaseId", "in": "path", "required": true, "schema": { "type": "string" } }
        ],
        "responses": { "200": { "description": "Unlinked" } }
      }
    },
    "/requirements/traceability": {
      "get": { "tags": ["Requirements"], "summary": "Get traceability matrix", "responses": { "200": { "description": "Matrix data" } } }
    },
    "/cookies": {
      "get": { "tags": ["Cookies"], "summary": "List cookies", "responses": { "200": { "description": "Cookie list" } } },
      "post": { "tags": ["Cookies"], "summary": "Set a cookie", "responses": { "201": { "description": "Created" } } },
      "delete": { "tags": ["Cookies"], "summary": "Clear all cookies", "responses": { "200": { "description": "Cleared" } } }
    },
    "/cookies/{id}": {
      "delete": {
        "tags": ["Cookies"],
        "summary": "Delete a single cookie",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Deleted" } }
      }
    },
    "/ws/connect": {
      "post": { "tags": ["WebSocket"], "summary": "Create WebSocket connection", "requestBody": { "content": { "application/json": { "schema": { "type": "object", "required": ["url"], "properties": { "url": { "type": "string" }, "headers": { "type": "object" }, "protocols": { "type": "array", "items": { "type": "string" } } } } } } }, "responses": { "200": { "description": "Connection ID" } } }
    },
    "/ws/send/{connectionId}": {
      "post": {
        "tags": ["WebSocket"],
        "summary": "Send message to WebSocket connection",
        "parameters": [{ "name": "connectionId", "in": "path", "required": true, "schema": { "type": "string" } }],
        "requestBody": { "content": { "application/json": { "schema": { "type": "object", "properties": { "message": { "type": "string" } } } } } },
        "responses": { "200": { "description": "Sent" } }
      }
    },
    "/ws/disconnect/{connectionId}": {
      "post": {
        "tags": ["WebSocket"],
        "summary": "Disconnect WebSocket connection",
        "parameters": [{ "name": "connectionId", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Disconnected" } }
      }
    },
    "/ws/history/{connectionId}": {
      "get": {
        "tags": ["WebSocket"],
        "summary": "Get message history for a WebSocket connection",
        "parameters": [{ "name": "connectionId", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "Array of messages" } }
      }
    },
    "/sse/connect": {
      "post": { "tags": ["SSE"], "summary": "Connect to SSE endpoint (POST)", "responses": { "200": { "description": "SSE stream" } } },
      "get": { "tags": ["SSE"], "summary": "Connect to SSE endpoint (GET)", "responses": { "200": { "description": "SSE stream" } } }
    },
    "/healthz": {
      "get": {
        "tags": ["Health"],
        "summary": "Liveness probe",
        "security": [],
        "description": "Returns 200 if the server is running. Used by Kubernetes liveness probe.",
        "responses": { "200": { "description": "Server is alive", "content": { "application/json": { "schema": { "type": "object", "properties": { "status": { "type": "string", "example": "ok" } } } } } } }
      }
    },
    "/readyz": {
      "get": {
        "tags": ["Health"],
        "summary": "Readiness probe",
        "security": [],
        "description": "Returns 200 if the server is ready to accept traffic (database connection is healthy). Used by Kubernetes readiness probe.",
        "responses": {
          "200": { "description": "Server is ready", "content": { "application/json": { "schema": { "type": "object", "properties": { "status": { "type": "string", "example": "ok" } } } } } },
          "503": { "description": "Server is not ready", "content": { "application/json": { "schema": { "type": "object", "properties": { "status": { "type": "string", "example": "error" }, "reason": { "type": "string" } } } } } }
        }
      }
    }
  }
}`
