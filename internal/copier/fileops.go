package copier

// preallocMin: smaller files are written in one or two calls anyway, so
// reserving their space first would only add a call.
const preallocMin = 1 << 20
