package model

// AccountType drives time aggregation, YTD behavior and FX rate selection.
type AccountType string

const (
	AccountRevenue      AccountType = "Revenue"
	AccountExpense      AccountType = "Expense"
	AccountAsset        AccountType = "Asset"
	AccountLiability    AccountType = "Liability"
	AccountEquity       AccountType = "Equity"
	AccountFlow         AccountType = "Flow"
	AccountNonFinancial AccountType = "NonFinancial"
)

// AccountTypes lists all valid account types.
var AccountTypes = []AccountType{
	AccountRevenue, AccountExpense, AccountAsset, AccountLiability,
	AccountEquity, AccountFlow, AccountNonFinancial,
}

// Valid reports whether t is a known account type ("" is not).
func (t AccountType) Valid() bool {
	for _, a := range AccountTypes {
		if t == a {
			return true
		}
	}
	return false
}

// IsBalance reports whether the type is a point-in-time balance
// (time agg = last value) rather than a flow (time agg = sum).
func (t AccountType) IsBalance() bool {
	switch t {
	case AccountAsset, AccountLiability, AccountEquity:
		return true
	}
	return false
}

// RateType returns the FX rate type used to translate this account.
func (t AccountType) RateType() RateType {
	if t.IsBalance() {
		return RateClosing
	}
	return RateAverage
}
