{{/*
Keep quantity arithmetic in decimal strings so comparisons do not lose precision
above float64's integer range. The scale is a base-10 exponent.
*/}}
{{- define "eviction-autoscaler.memoryDigits" -}}
{{- $digits := .digits -}}
{{- $carry := .carry | default 0 -}}
{{- $result := "" -}}
{{- range $i := until (len $digits) -}}
  {{- $index := int (sub (len $digits) (add $i 1)) -}}
  {{- $digit := atoi (substr $index (add1 $index | int) $digits) -}}
  {{- $value := add (mul $digit $.factor) $carry -}}
  {{- $result = printf "%d%s" (mod $value 10) $result -}}
  {{- $carry = div $value 10 -}}
{{- end -}}
{{- if gt $carry 0 -}}{{- $result = printf "%d%s" $carry $result -}}{{- end -}}
{{- $result -}}
{{- end -}}

{{- define "eviction-autoscaler.memoryGreater" -}}
{{- $left := .left -}}
{{- $right := .right -}}
{{- $greater := false -}}
{{- if ne $left.digits "0" -}}
  {{- if eq $right.digits "0" -}}
    {{- $greater = true -}}
  {{- else -}}
    {{- $leftOrder := add (len $left.digits) $left.scale -}}
    {{- $rightOrder := add (len $right.digits) $right.scale -}}
    {{- if eq $leftOrder $rightOrder -}}
      {{- $width := max (len $left.digits) (len $right.digits) -}}
      {{- $leftDigits := printf "%s%s" $left.digits (repeat (sub $width (len $left.digits) | int) "0") -}}
      {{- $rightDigits := printf "%s%s" $right.digits (repeat (sub $width (len $right.digits) | int) "0") -}}
      {{- $greater = gt $leftDigits $rightDigits -}}
    {{- else -}}
      {{- $greater = gt $leftOrder $rightOrder -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- $greater -}}
{{- end -}}

{{- define "eviction-autoscaler.memoryQuantity" -}}
{{- $raw := toString .value -}}
{{- $error := printf "%s must be a non-negative Kubernetes memory quantity, got %q" .field $raw -}}
{{- if not (regexMatch "^[-+]?([0-9]+([.][0-9]*)?|[.][0-9]+)([eE][-+]?[0-9]+|[numkMGTPE]|[KMGTPE]i)?$" $raw) -}}
  {{- fail $error -}}
{{- end -}}
{{- $number := regexFind "^[-+]?([0-9]+([.][0-9]*)?|[.][0-9]+)" $raw -}}
{{- $suffix := trimPrefix $number $raw -}}
{{- $digits := regexReplaceAll "^0+" (replace "." "" (trimPrefix "-" (trimPrefix "+" $number))) "" -}}
{{- if and (hasPrefix "-" $number) (ne $digits "") -}}{{- fail $error -}}{{- end -}}
{{- if eq $digits "" -}}{{- $digits = "0" -}}{{- end -}}
{{- $scale := int64 0 -}}
{{- if contains "." $number -}}
  {{- $scale = sub 0 (len (index (splitList "." $number) 1)) -}}
{{- end -}}
{{- $decimal := dict "" 0 "n" -9 "u" -6 "m" -3 "k" 3 "M" 6 "G" 9 "T" 12 "P" 15 "E" 18 -}}
{{- $binary := dict "Ki" 1 "Mi" 2 "Gi" 3 "Ti" 4 "Pi" 5 "Ei" 6 -}}
{{- if hasKey $binary $suffix -}}
  {{- range until (get $binary $suffix | int) -}}
    {{- $digits = include "eviction-autoscaler.memoryDigits" (dict "digits" $digits "factor" 1024) -}}
  {{- end -}}
{{- else if hasKey $decimal $suffix -}}
  {{- $scale = add $scale (get $decimal $suffix) -}}
{{- else -}}
  {{- $exponent := substr 1 (len $suffix) $suffix -}}
  {{- $exponentDigits := regexReplaceAll "^0+" (trimPrefix "-" (trimPrefix "+" $exponent)) "" -}}
  {{- if gt (len $exponentDigits) 10 -}}{{- fail $error -}}{{- end -}}
  {{- $exponentValue := atoi $exponentDigits | int64 -}}
  {{- $maximum := int64 2147483647 -}}
  {{- if hasPrefix "-" $exponent -}}{{- $maximum = int64 2147483648 -}}{{- end -}}
  {{- if gt $exponentValue $maximum -}}{{- fail $error -}}{{- end -}}
  {{- if hasPrefix "-" $exponent -}}{{- $exponentValue = mul -1 $exponentValue -}}{{- end -}}
  {{- $scale = add $scale $exponentValue -}}
{{- end -}}
{{/* Kubernetes rounds nonzero quantities up to nano precision. */}}
{{- if and (ne $digits "0") (lt $scale -9) -}}
  {{- $discard := sub -9 $scale -}}
  {{- if ge $discard (len $digits) -}}
    {{- $digits = "1" -}}
  {{- else -}}
    {{- $split := sub (len $digits) $discard | int -}}
    {{- $tail := substr $split (len $digits) $digits -}}
    {{- $digits = substr 0 $split $digits -}}
    {{- if regexMatch "[1-9]" $tail -}}
      {{- $digits = include "eviction-autoscaler.memoryDigits" (dict "digits" $digits "factor" 1 "carry" 1) -}}
    {{- end -}}
  {{- end -}}
  {{- $scale = int64 -9 -}}
{{- end -}}
{{- $quantity := dict "digits" $digits "scale" $scale -}}
{{/* Kubernetes caps BinarySI quantities at the maximum signed 64-bit value. */}}
{{- $maximum := dict "digits" "9223372036854775807" "scale" 0 -}}
{{- if and (hasKey $binary $suffix) (eq (include "eviction-autoscaler.memoryGreater" (dict "left" $quantity "right" $maximum)) "true") -}}
  {{- $quantity = $maximum -}}
{{- end -}}
{{- toJson $quantity -}}
{{- end -}}

{{- define "eviction-autoscaler.validateMemoryResources" -}}
{{- $resources := .Values.resources | default dict -}}
{{- $quantities := dict -}}
{{- range $section := list "requests" "limits" -}}
  {{- $settings := get $resources $section | default dict -}}
  {{- if and (hasKey $settings "memory") (ne (get $settings "memory") nil) -}}
    {{- $quantity := include "eviction-autoscaler.memoryQuantity" (dict "field" (printf "resources.%s.memory" $section) "value" (get $settings "memory")) | fromJson -}}
    {{- $_ := set $quantities $section $quantity -}}
  {{- end -}}
{{- end -}}
{{- if and (hasKey $quantities "requests") (hasKey $quantities "limits") -}}
  {{- if eq (include "eviction-autoscaler.memoryGreater" (dict "left" $quantities.requests "right" $quantities.limits)) "true" -}}
    {{- fail (printf "resources.requests.memory (%v) must not exceed resources.limits.memory (%v); set both values when lowering the memory limit" $resources.requests.memory $resources.limits.memory) -}}
  {{- end -}}
{{- end -}}
{{- end -}}
