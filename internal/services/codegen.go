package services

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

type CodegenRequest struct {
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	Params   map[string]string `json:"params"`
	Body     *RequestBody      `json:"body"`
	Auth     *RequestAuth      `json:"auth"`
	Language string            `json:"language"`
}

func GenerateCode(req *CodegenRequest) (string, error) {
	// Build full URL with params
	fullURL := buildFullURL(req.URL, req.Params, req.Auth)

	// Build effective headers (including auth)
	headers := buildEffectiveHeaders(req.Headers, req.Auth, req.Body)

	var bodyContent string
	if req.Body != nil && req.Body.Type != "none" {
		bodyContent = req.Body.Content
	}

	switch req.Language {
	case "curl":
		return generateCurl(req.Method, fullURL, headers, bodyContent), nil
	case "javascript_fetch":
		return generateJSFetch(req.Method, fullURL, headers, bodyContent), nil
	case "javascript_axios":
		return generateJSAxios(req.Method, fullURL, headers, bodyContent), nil
	case "python_requests":
		return generatePython(req.Method, fullURL, headers, bodyContent), nil
	case "go":
		return generateGo(req.Method, fullURL, headers, bodyContent), nil
	case "java":
		return generateJava(req.Method, fullURL, headers, bodyContent), nil
	case "csharp":
		return generateCSharp(req.Method, fullURL, headers, bodyContent), nil
	case "php_curl":
		return generatePHP(req.Method, fullURL, headers, bodyContent), nil
	case "ruby":
		return generateRuby(req.Method, fullURL, headers, bodyContent), nil
	case "swift":
		return generateSwift(req.Method, fullURL, headers, bodyContent), nil
	case "kotlin":
		return generateKotlin(req.Method, fullURL, headers, bodyContent), nil
	case "rust":
		return generateRust(req.Method, fullURL, headers, bodyContent), nil
	case "dart":
		return generateDart(req.Method, fullURL, headers, bodyContent), nil
	default:
		return "", fmt.Errorf("unsupported language: %s", req.Language)
	}
}

func buildFullURL(rawURL string, params map[string]string, auth *RequestAuth) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	if auth != nil && auth.Type == "apikey" && auth.AddTo == "query" {
		q.Set(auth.Key, auth.Value)
	}
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func buildEffectiveHeaders(headers map[string]string, auth *RequestAuth, body *RequestBody) map[string]string {
	effective := make(map[string]string)
	for k, v := range headers {
		effective[k] = v
	}

	if body != nil && effective["Content-Type"] == "" {
		switch body.Type {
		case "json", "graphql":
			effective["Content-Type"] = "application/json"
		case "xml":
			effective["Content-Type"] = "application/xml"
		case "form":
			effective["Content-Type"] = "application/x-www-form-urlencoded"
		}
	}

	// SOAP derives Content-Type + SOAPAction from the version (same as the FE
	// send path) so generated snippets don't fall back to form-encoding and
	// get a 415 from the SOAP server.
	if body != nil && body.Type == "soap" {
		version := body.SoapVersion
		if version == "" {
			version = "1.1"
		}
		action := strings.TrimSpace(body.SoapAction)
		hasHeader := func(name string) bool {
			for k := range effective {
				if strings.EqualFold(k, name) {
					return true
				}
			}
			return false
		}
		if !hasHeader("Content-Type") {
			if version == "1.2" {
				if action != "" {
					effective["Content-Type"] = `application/soap+xml; charset=utf-8; action="` + action + `"`
				} else {
					effective["Content-Type"] = "application/soap+xml; charset=utf-8"
				}
			} else {
				effective["Content-Type"] = "text/xml; charset=utf-8"
			}
		}
		if version == "1.1" && !hasHeader("SOAPAction") {
			if action != "" {
				effective["SOAPAction"] = `"` + action + `"`
			} else {
				effective["SOAPAction"] = `""`
			}
		}
	}

	if auth != nil {
		switch auth.Type {
		case "bearer":
			effective["Authorization"] = "Bearer " + auth.Token
		case "basic":
			cred := base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Password))
			effective["Authorization"] = "Basic " + cred
		case "apikey":
			if auth.AddTo != "query" {
				effective[auth.Key] = auth.Value
			}
		}
	}

	return effective
}

func sortedHeaderKeys(headers map[string]string) []string {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func escapeString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func escapeSingleQuote(s string) string {
	return strings.ReplaceAll(s, `'`, `'\''`)
}

// --- cURL ---
func generateCurl(method, fullURL string, headers map[string]string, body string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("curl -X %s '%s'", strings.ToUpper(method), escapeSingleQuote(fullURL)))

	for _, k := range sortedHeaderKeys(headers) {
		b.WriteString(fmt.Sprintf(" \\\n  -H '%s: %s'", escapeSingleQuote(k), escapeSingleQuote(headers[k])))
	}

	if body != "" {
		b.WriteString(fmt.Sprintf(" \\\n  -d '%s'", escapeSingleQuote(body)))
	}

	return b.String()
}

// --- JavaScript Fetch ---
func generateJSFetch(method, fullURL string, headers map[string]string, body string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("const response = await fetch('%s', {\n", escapeString(fullURL)))
	b.WriteString(fmt.Sprintf("  method: '%s',\n", strings.ToUpper(method)))

	if len(headers) > 0 {
		b.WriteString("  headers: {\n")
		for _, k := range sortedHeaderKeys(headers) {
			b.WriteString(fmt.Sprintf("    '%s': '%s',\n", escapeString(k), escapeString(headers[k])))
		}
		b.WriteString("  },\n")
	}

	if body != "" {
		b.WriteString(fmt.Sprintf("  body: '%s',\n", escapeString(body)))
	}

	b.WriteString("});\n\n")
	b.WriteString("const data = await response.text();\n")
	b.WriteString("console.log(response.status, data);")

	return b.String()
}

// --- JavaScript Axios ---
func generateJSAxios(method, fullURL string, headers map[string]string, body string) string {
	var b strings.Builder
	b.WriteString("const axios = require('axios');\n\n")
	b.WriteString("const config = {\n")
	b.WriteString(fmt.Sprintf("  method: '%s',\n", strings.ToLower(method)))
	b.WriteString(fmt.Sprintf("  url: '%s',\n", escapeString(fullURL)))

	if len(headers) > 0 {
		b.WriteString("  headers: {\n")
		for _, k := range sortedHeaderKeys(headers) {
			b.WriteString(fmt.Sprintf("    '%s': '%s',\n", escapeString(k), escapeString(headers[k])))
		}
		b.WriteString("  },\n")
	}

	if body != "" {
		b.WriteString(fmt.Sprintf("  data: '%s',\n", escapeString(body)))
	}

	b.WriteString("};\n\n")
	b.WriteString("axios(config)\n")
	b.WriteString("  .then(response => console.log(response.status, response.data))\n")
	b.WriteString("  .catch(error => console.error(error));")

	return b.String()
}

// --- Python Requests ---
func generatePython(method, fullURL string, headers map[string]string, body string) string {
	var b strings.Builder
	b.WriteString("import requests\n\n")
	b.WriteString(fmt.Sprintf("url = \"%s\"\n", escapeString(fullURL)))

	if len(headers) > 0 {
		b.WriteString("headers = {\n")
		for _, k := range sortedHeaderKeys(headers) {
			b.WriteString(fmt.Sprintf("    \"%s\": \"%s\",\n", escapeString(k), escapeString(headers[k])))
		}
		b.WriteString("}\n")
	}

	if body != "" {
		b.WriteString(fmt.Sprintf("payload = \"%s\"\n", escapeString(body)))
	}

	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("response = requests.%s(url", strings.ToLower(method)))

	if len(headers) > 0 {
		b.WriteString(", headers=headers")
	}
	if body != "" {
		b.WriteString(", data=payload")
	}
	b.WriteString(")\n\n")
	b.WriteString("print(response.status_code)\n")
	b.WriteString("print(response.text)")

	return b.String()
}

// --- Go ---
func generateGo(method, fullURL string, headers map[string]string, body string) string {
	var b strings.Builder
	b.WriteString("package main\n\n")
	b.WriteString("import (\n")
	b.WriteString("\t\"fmt\"\n")
	b.WriteString("\t\"io\"\n")
	b.WriteString("\t\"net/http\"\n")
	if body != "" {
		b.WriteString("\t\"strings\"\n")
	}
	b.WriteString(")\n\n")
	b.WriteString("func main() {\n")

	if body != "" {
		b.WriteString(fmt.Sprintf("\tbody := strings.NewReader(`%s`)\n", body))
		b.WriteString(fmt.Sprintf("\treq, err := http.NewRequest(\"%s\", \"%s\", body)\n", strings.ToUpper(method), escapeString(fullURL)))
	} else {
		b.WriteString(fmt.Sprintf("\treq, err := http.NewRequest(\"%s\", \"%s\", nil)\n", strings.ToUpper(method), escapeString(fullURL)))
	}

	b.WriteString("\tif err != nil {\n\t\tpanic(err)\n\t}\n\n")

	for _, k := range sortedHeaderKeys(headers) {
		b.WriteString(fmt.Sprintf("\treq.Header.Set(\"%s\", \"%s\")\n", escapeString(k), escapeString(headers[k])))
	}

	b.WriteString("\n\tclient := &http.Client{}\n")
	b.WriteString("\tresp, err := client.Do(req)\n")
	b.WriteString("\tif err != nil {\n\t\tpanic(err)\n\t}\n")
	b.WriteString("\tdefer resp.Body.Close()\n\n")
	b.WriteString("\trespBody, _ := io.ReadAll(resp.Body)\n")
	b.WriteString("\tfmt.Println(resp.StatusCode)\n")
	b.WriteString("\tfmt.Println(string(respBody))\n")
	b.WriteString("}")

	return b.String()
}

// --- Java ---
func generateJava(method, fullURL string, headers map[string]string, body string) string {
	var b strings.Builder
	b.WriteString("import java.net.URI;\n")
	b.WriteString("import java.net.http.HttpClient;\n")
	b.WriteString("import java.net.http.HttpRequest;\n")
	b.WriteString("import java.net.http.HttpResponse;\n\n")
	b.WriteString("public class Main {\n")
	b.WriteString("    public static void main(String[] args) throws Exception {\n")
	b.WriteString("        HttpClient client = HttpClient.newHttpClient();\n\n")

	if body != "" {
		b.WriteString(fmt.Sprintf("        HttpRequest request = HttpRequest.newBuilder()\n"))
		b.WriteString(fmt.Sprintf("            .uri(URI.create(\"%s\"))\n", escapeString(fullURL)))
		b.WriteString(fmt.Sprintf("            .method(\"%s\", HttpRequest.BodyPublishers.ofString(\"%s\"))\n",
			strings.ToUpper(method), escapeString(body)))
	} else {
		b.WriteString(fmt.Sprintf("        HttpRequest request = HttpRequest.newBuilder()\n"))
		b.WriteString(fmt.Sprintf("            .uri(URI.create(\"%s\"))\n", escapeString(fullURL)))
		if strings.ToUpper(method) == "GET" {
			b.WriteString("            .GET()\n")
		} else {
			b.WriteString(fmt.Sprintf("            .method(\"%s\", HttpRequest.BodyPublishers.noBody())\n", strings.ToUpper(method)))
		}
	}

	for _, k := range sortedHeaderKeys(headers) {
		b.WriteString(fmt.Sprintf("            .header(\"%s\", \"%s\")\n", escapeString(k), escapeString(headers[k])))
	}

	b.WriteString("            .build();\n\n")
	b.WriteString("        HttpResponse<String> response = client.send(request, HttpResponse.BodyHandlers.ofString());\n")
	b.WriteString("        System.out.println(response.statusCode());\n")
	b.WriteString("        System.out.println(response.body());\n")
	b.WriteString("    }\n")
	b.WriteString("}")

	return b.String()
}

// --- C# ---
func generateCSharp(method, fullURL string, headers map[string]string, body string) string {
	var b strings.Builder
	b.WriteString("using System;\n")
	b.WriteString("using System.Net.Http;\n")
	b.WriteString("using System.Threading.Tasks;\n\n")
	b.WriteString("class Program\n{\n")
	b.WriteString("    static async Task Main(string[] args)\n    {\n")
	b.WriteString("        using var client = new HttpClient();\n\n")

	b.WriteString(fmt.Sprintf("        var request = new HttpRequestMessage(HttpMethod.%s, \"%s\");\n",
		capitalizeMethod(method), escapeString(fullURL)))

	for _, k := range sortedHeaderKeys(headers) {
		if strings.ToLower(k) == "content-type" {
			continue // Content-Type is set on content, not request
		}
		b.WriteString(fmt.Sprintf("        request.Headers.TryAddWithoutValidation(\"%s\", \"%s\");\n",
			escapeString(k), escapeString(headers[k])))
	}

	if body != "" {
		ct := headers["Content-Type"]
		if ct == "" {
			ct = "application/json"
		}
		b.WriteString(fmt.Sprintf("        request.Content = new StringContent(\"%s\", System.Text.Encoding.UTF8, \"%s\");\n",
			escapeString(body), ct))
	}

	b.WriteString("\n        var response = await client.SendAsync(request);\n")
	b.WriteString("        var responseBody = await response.Content.ReadAsStringAsync();\n")
	b.WriteString("        Console.WriteLine((int)response.StatusCode);\n")
	b.WriteString("        Console.WriteLine(responseBody);\n")
	b.WriteString("    }\n}")

	return b.String()
}

func capitalizeMethod(method string) string {
	m := strings.ToLower(method)
	switch m {
	case "get":
		return "Get"
	case "post":
		return "Post"
	case "put":
		return "Put"
	case "delete":
		return "Delete"
	case "patch":
		return "Patch"
	case "head":
		return "Head"
	case "options":
		return "Options"
	default:
		if len(m) == 0 {
			return m
		}
		return strings.ToUpper(m[:1]) + m[1:]
	}
}

// --- PHP ---
func generatePHP(method, fullURL string, headers map[string]string, body string) string {
	var b strings.Builder
	b.WriteString("<?php\n\n")
	b.WriteString("$ch = curl_init();\n\n")
	b.WriteString(fmt.Sprintf("curl_setopt($ch, CURLOPT_URL, '%s');\n", escapeSingleQuote(fullURL)))
	b.WriteString("curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);\n")
	b.WriteString(fmt.Sprintf("curl_setopt($ch, CURLOPT_CUSTOMREQUEST, '%s');\n", strings.ToUpper(method)))

	if len(headers) > 0 {
		b.WriteString("curl_setopt($ch, CURLOPT_HTTPHEADER, [\n")
		for _, k := range sortedHeaderKeys(headers) {
			b.WriteString(fmt.Sprintf("    '%s: %s',\n", escapeSingleQuote(k), escapeSingleQuote(headers[k])))
		}
		b.WriteString("]);\n")
	}

	if body != "" {
		b.WriteString(fmt.Sprintf("curl_setopt($ch, CURLOPT_POSTFIELDS, '%s');\n", escapeSingleQuote(body)))
	}

	b.WriteString("\n$response = curl_exec($ch);\n")
	b.WriteString("$httpCode = curl_getinfo($ch, CURLINFO_HTTP_CODE);\n")
	b.WriteString("curl_close($ch);\n\n")
	b.WriteString("echo $httpCode . \"\\n\";\n")
	b.WriteString("echo $response;\n")

	return b.String()
}

// --- Ruby ---
func generateRuby(method, fullURL string, headers map[string]string, body string) string {
	var b strings.Builder
	b.WriteString("require 'net/http'\n")
	b.WriteString("require 'uri'\n\n")
	b.WriteString(fmt.Sprintf("uri = URI.parse('%s')\n", escapeSingleQuote(fullURL)))

	rubyClass := "Net::HTTP::Get"
	switch strings.ToUpper(method) {
	case "POST":
		rubyClass = "Net::HTTP::Post"
	case "PUT":
		rubyClass = "Net::HTTP::Put"
	case "PATCH":
		rubyClass = "Net::HTTP::Patch"
	case "DELETE":
		rubyClass = "Net::HTTP::Delete"
	case "HEAD":
		rubyClass = "Net::HTTP::Head"
	case "OPTIONS":
		rubyClass = "Net::HTTP::Options"
	}

	b.WriteString(fmt.Sprintf("request = %s.new(uri)\n", rubyClass))

	for _, k := range sortedHeaderKeys(headers) {
		b.WriteString(fmt.Sprintf("request['%s'] = '%s'\n", escapeSingleQuote(k), escapeSingleQuote(headers[k])))
	}

	if body != "" {
		b.WriteString(fmt.Sprintf("request.body = '%s'\n", escapeSingleQuote(body)))
	}

	b.WriteString("\nhttp = Net::HTTP.new(uri.host, uri.port)\n")
	b.WriteString("http.use_ssl = (uri.scheme == 'https')\n")
	b.WriteString("response = http.request(request)\n\n")
	b.WriteString("puts response.code\n")
	b.WriteString("puts response.body")

	return b.String()
}

// --- Swift ---
func generateSwift(method, fullURL string, headers map[string]string, body string) string {
	var b strings.Builder
	b.WriteString("import Foundation\n\n")
	b.WriteString(fmt.Sprintf("let url = URL(string: \"%s\")!\n", escapeString(fullURL)))
	b.WriteString("var request = URLRequest(url: url)\n")
	b.WriteString(fmt.Sprintf("request.httpMethod = \"%s\"\n", strings.ToUpper(method)))

	for _, k := range sortedHeaderKeys(headers) {
		b.WriteString(fmt.Sprintf("request.setValue(\"%s\", forHTTPHeaderField: \"%s\")\n",
			escapeString(headers[k]), escapeString(k)))
	}

	if body != "" {
		b.WriteString(fmt.Sprintf("request.httpBody = \"%s\".data(using: .utf8)\n", escapeString(body)))
	}

	b.WriteString("\nlet semaphore = DispatchSemaphore(value: 0)\n\n")
	b.WriteString("let task = URLSession.shared.dataTask(with: request) { data, response, error in\n")
	b.WriteString("    if let error = error {\n")
	b.WriteString("        print(\"Error: \\(error)\")\n")
	b.WriteString("        semaphore.signal()\n")
	b.WriteString("        return\n")
	b.WriteString("    }\n")
	b.WriteString("    if let httpResponse = response as? HTTPURLResponse {\n")
	b.WriteString("        print(httpResponse.statusCode)\n")
	b.WriteString("    }\n")
	b.WriteString("    if let data = data, let body = String(data: data, encoding: .utf8) {\n")
	b.WriteString("        print(body)\n")
	b.WriteString("    }\n")
	b.WriteString("    semaphore.signal()\n")
	b.WriteString("}\n\n")
	b.WriteString("task.resume()\n")
	b.WriteString("semaphore.wait()")

	return b.String()
}

// --- Kotlin ---
func generateKotlin(method, fullURL string, headers map[string]string, body string) string {
	var b strings.Builder
	b.WriteString("import okhttp3.OkHttpClient\n")
	b.WriteString("import okhttp3.Request\n")
	if body != "" {
		b.WriteString("import okhttp3.RequestBody.Companion.toRequestBody\n")
		b.WriteString("import okhttp3.MediaType.Companion.toMediaType\n")
	}
	b.WriteString("\nfun main() {\n")
	b.WriteString("    val client = OkHttpClient()\n\n")

	if body != "" {
		ct := headers["Content-Type"]
		if ct == "" {
			ct = "application/json"
		}
		b.WriteString(fmt.Sprintf("    val body = \"%s\".toRequestBody(\"%s\".toMediaType())\n\n",
			escapeString(body), ct))
	}

	b.WriteString("    val request = Request.Builder()\n")
	b.WriteString(fmt.Sprintf("        .url(\"%s\")\n", escapeString(fullURL)))

	if body != "" {
		b.WriteString(fmt.Sprintf("        .method(\"%s\", body)\n", strings.ToUpper(method)))
	} else {
		if strings.ToUpper(method) == "GET" {
			b.WriteString("        .get()\n")
		} else {
			b.WriteString(fmt.Sprintf("        .method(\"%s\", null)\n", strings.ToUpper(method)))
		}
	}

	for _, k := range sortedHeaderKeys(headers) {
		b.WriteString(fmt.Sprintf("        .addHeader(\"%s\", \"%s\")\n", escapeString(k), escapeString(headers[k])))
	}

	b.WriteString("        .build()\n\n")
	b.WriteString("    val response = client.newCall(request).execute()\n")
	b.WriteString("    println(response.code)\n")
	b.WriteString("    println(response.body?.string())\n")
	b.WriteString("}")

	return b.String()
}

// --- Rust ---
func generateRust(method, fullURL string, headers map[string]string, body string) string {
	var b strings.Builder
	b.WriteString("use reqwest;\n\n")
	b.WriteString("#[tokio::main]\n")
	b.WriteString("async fn main() -> Result<(), Box<dyn std::error::Error>> {\n")
	b.WriteString("    let client = reqwest::Client::new();\n\n")

	b.WriteString(fmt.Sprintf("    let response = client.%s(\"%s\")\n",
		strings.ToLower(method), escapeString(fullURL)))

	for _, k := range sortedHeaderKeys(headers) {
		b.WriteString(fmt.Sprintf("        .header(\"%s\", \"%s\")\n", escapeString(k), escapeString(headers[k])))
	}

	if body != "" {
		b.WriteString(fmt.Sprintf("        .body(\"%s\")\n", escapeString(body)))
	}

	b.WriteString("        .send()\n")
	b.WriteString("        .await?;\n\n")
	b.WriteString("    println!(\"{}\", response.status());\n")
	b.WriteString("    println!(\"{}\", response.text().await?);\n\n")
	b.WriteString("    Ok(())\n")
	b.WriteString("}")

	return b.String()
}

// --- Dart ---
func generateDart(method, fullURL string, headers map[string]string, body string) string {
	var b strings.Builder
	b.WriteString("import 'package:http/http.dart' as http;\n\n")
	b.WriteString("void main() async {\n")
	b.WriteString(fmt.Sprintf("  final url = Uri.parse('%s');\n\n", escapeSingleQuote(fullURL)))

	if len(headers) > 0 {
		b.WriteString("  final headers = {\n")
		for _, k := range sortedHeaderKeys(headers) {
			b.WriteString(fmt.Sprintf("    '%s': '%s',\n", escapeSingleQuote(k), escapeSingleQuote(headers[k])))
		}
		b.WriteString("  };\n\n")
	}

	m := strings.ToLower(method)
	if body != "" {
		b.WriteString(fmt.Sprintf("  final body = '%s';\n\n", escapeSingleQuote(body)))
		if len(headers) > 0 {
			b.WriteString(fmt.Sprintf("  final response = await http.%s(url, headers: headers, body: body);\n", m))
		} else {
			b.WriteString(fmt.Sprintf("  final response = await http.%s(url, body: body);\n", m))
		}
	} else {
		if len(headers) > 0 {
			b.WriteString(fmt.Sprintf("  final response = await http.%s(url, headers: headers);\n", m))
		} else {
			b.WriteString(fmt.Sprintf("  final response = await http.%s(url);\n", m))
		}
	}

	b.WriteString("\n  print(response.statusCode);\n")
	b.WriteString("  print(response.body);\n")
	b.WriteString("}")

	return b.String()
}
