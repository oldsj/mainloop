package health

// ReadOnly runs fn in the transaction the routes read in.
var ReadOnly = (*Service).readOnly
