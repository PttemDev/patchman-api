package handlers

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// JUnit XML structures for parsing

type junitTestSuites struct {
	XMLName    xml.Name         `xml:"testsuites"`
	TestSuites []junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	XMLName   xml.Name        `xml:"testsuite"`
	Name      string          `xml:"name,attr"`
	Tests     int             `xml:"tests,attr"`
	Failures  int             `xml:"failures,attr"`
	Errors    int             `xml:"errors,attr"`
	Skipped   int             `xml:"skipped,attr"`
	Time      float64         `xml:"time,attr"`
	TestCases []junitTestCase `xml:"testcase"`
}

type junitTestCase struct {
	XMLName   xml.Name      `xml:"testcase"`
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Time      float64       `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure"`
	Error     *junitError   `xml:"error"`
	Skipped   *junitSkipped `xml:"skipped"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

type junitError struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}

// JSON output structures for storing parsed details

type suiteDetail struct {
	Name      string           `json:"name"`
	Tests     int              `json:"tests"`
	Failures  int              `json:"failures"`
	Errors    int              `json:"errors"`
	Skipped   int              `json:"skipped"`
	Time      float64          `json:"time"`
	TestCases []testCaseDetail `json:"testCases"`
}

type testCaseDetail struct {
	Name      string `json:"name"`
	ClassName string `json:"className"`
	Time      float64 `json:"time"`
	Status    string `json:"status"` // passed, failed, error, skipped
	Message   string `json:"message,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type JUnitImportHandler struct {
	db *gorm.DB
}

func NewJUnitImportHandler(db *gorm.DB) *JUnitImportHandler {
	return &JUnitImportHandler{db: db}
}

// Import handles POST /api/import/junit
// Accepts multipart/form-data with a "file" field containing JUnit XML.
// Optional query param: testPlanId to link results to a test plan.
func (h *JUnitImportHandler) Import(c *gin.Context) {
	userID := c.GetString("userID")

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "File upload required (field: file)"})
		return
	}
	defer file.Close()

	xmlBytes, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read uploaded file"})
		return
	}

	// Try parsing as <testsuites> wrapper first
	var suites junitTestSuites
	if err := xml.Unmarshal(xmlBytes, &suites); err != nil {
		// Try parsing as a single <testsuite> (some tools emit this without a wrapper)
		var single junitTestSuite
		if err2 := xml.Unmarshal(xmlBytes, &single); err2 != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse JUnit XML: " + err.Error()})
			return
		}
		suites.TestSuites = []junitTestSuite{single}
	}

	// Aggregate stats and build detail
	var totalTests, totalPassed, totalFailed, totalSkipped, totalErrors int
	var totalDuration float64
	var details []suiteDetail

	for _, suite := range suites.TestSuites {
		sd := suiteDetail{
			Name:     suite.Name,
			Tests:    suite.Tests,
			Failures: suite.Failures,
			Errors:   suite.Errors,
			Skipped:  suite.Skipped,
			Time:     suite.Time,
		}

		suitePassCount := 0
		suiteFailCount := 0
		suiteErrorCount := 0
		suiteSkipCount := 0

		for _, tc := range suite.TestCases {
			tcd := testCaseDetail{
				Name:      tc.Name,
				ClassName: tc.ClassName,
				Time:      tc.Time,
			}

			if tc.Failure != nil {
				tcd.Status = "failed"
				tcd.Message = tc.Failure.Message
				tcd.Detail = tc.Failure.Body
				suiteFailCount++
			} else if tc.Error != nil {
				tcd.Status = "error"
				tcd.Message = tc.Error.Message
				tcd.Detail = tc.Error.Body
				suiteErrorCount++
			} else if tc.Skipped != nil {
				tcd.Status = "skipped"
				tcd.Message = tc.Skipped.Message
				suiteSkipCount++
			} else {
				tcd.Status = "passed"
				suitePassCount++
			}

			sd.TestCases = append(sd.TestCases, tcd)
		}

		// Use XML attributes when available, fall back to counting
		if suite.Tests > 0 {
			totalTests += suite.Tests
		} else {
			totalTests += len(suite.TestCases)
		}

		totalFailed += suiteFailCount
		totalErrors += suiteErrorCount
		totalSkipped += suiteSkipCount
		totalPassed += suitePassCount
		totalDuration += suite.Time

		details = append(details, sd)
	}

	suitesJSON, _ := json.Marshal(details)

	testPlanID := c.Query("testPlanId")

	result := models.JUnitImportResult{
		ID:         uuid.New().String(),
		UserID:     userID,
		TestPlanID: testPlanID,
		FileName:   header.Filename,
		TotalTests: totalTests,
		Passed:     totalPassed,
		Failed:     totalFailed,
		Skipped:    totalSkipped,
		Errors:     totalErrors,
		Duration:   totalDuration,
		Suites:     string(suitesJSON),
		ImportedAt: time.Now(),
	}

	if err := h.db.Create(&result).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save import result"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": result})
}

// List handles GET /api/import/junit
// Returns all JUnit import results for the authenticated user.
func (h *JUnitImportHandler) List(c *gin.Context) {
	userID := c.GetString("userID")

	var results []models.JUnitImportResult
	query := h.db.Where("user_id = ?", userID).Order("imported_at DESC")

	testPlanID := c.Query("testPlanId")
	if testPlanID != "" {
		query = query.Where("test_plan_id = ?", testPlanID)
	}

	if err := query.Find(&results).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch import results"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": results})
}

// Get handles GET /api/import/junit/:id
// Returns a single JUnit import result with full suite detail.
func (h *JUnitImportHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var result models.JUnitImportResult
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&result).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Import result not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": result})
}

// Delete handles DELETE /api/import/junit/:id
func (h *JUnitImportHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var result models.JUnitImportResult
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&result).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Import result not found"})
		return
	}

	if err := h.db.Delete(&result).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete import result"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Import result deleted"}})
}
