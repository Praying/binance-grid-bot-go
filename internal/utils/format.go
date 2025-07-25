package utils

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// FormatPrice formats a float64 price according to a given tickSize.
// It returns a string representation of the formatted price.
func FormatPrice(price float64, tickSize string) (string, error) {
	tickSizeFloat, err := strconv.ParseFloat(tickSize, 64)
	if err != nil {
		return "", fmt.Errorf("could not parse tickSize '%s': %v", tickSize, err)
	}
	if tickSizeFloat <= 0 {
		return "", fmt.Errorf("tickSize must be positive, but got %f", tickSizeFloat)
	}

	parts := strings.Split(tickSize, ".")
	var decimals int
	if len(parts) == 2 {
		trimmed := strings.TrimRight(parts[1], "0")
		decimals = len(trimmed)
	} else {
		decimals = 0
	}

	value := math.Floor(price/tickSizeFloat) * tickSizeFloat

	return strconv.FormatFloat(value, 'f', decimals, 64), nil
}

// FormatQuantity formats a float64 quantity according to a given stepSize.
// It returns a string representation of the formatted quantity.
func FormatQuantity(quantity float64, stepSize string) (string, error) {
	stepSizeFloat, err := strconv.ParseFloat(stepSize, 64)
	if err != nil {
		return "", fmt.Errorf("could not parse stepSize '%s': %v", stepSize, err)
	}
	if stepSizeFloat <= 0 {
		return "", fmt.Errorf("stepSize must be positive, but got %f", stepSizeFloat)
	}

	parts := strings.Split(stepSize, ".")
	var decimals int
	if len(parts) == 2 {
		trimmed := strings.TrimRight(parts[1], "0")
		decimals = len(trimmed)
	} else {
		decimals = 0
	}

	value := math.Floor(quantity/stepSizeFloat) * stepSizeFloat

	return strconv.FormatFloat(value, 'f', decimals, 64), nil
}
