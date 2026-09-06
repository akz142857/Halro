#!/usr/bin/env ruby
# frozen_string_literal: true

abort "usage: update-formula.rb VERSION CHECKSUMS FORMULA" unless ARGV.length == 3

version, checksums_path, formula_path = ARGV
abort "invalid version: #{version}" unless version.match?(/\Av\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?\z/)

checksums = File.readlines(checksums_path, chomp: true).to_h do |line|
  digest, asset = line.split
  [asset&.sub(/^\*/, ""), digest]
end
assets = %w[
  halro-darwin-amd64.tar.gz
  halro-darwin-arm64.tar.gz
  halro-linux-amd64.tar.gz
  halro-linux-arm64.tar.gz
]
assets.each do |asset|
  digest = checksums[asset]
  abort "missing SHA-256 for #{asset}" unless digest&.match?(/\A[0-9a-f]{64}\z/)
end

formula = File.read(formula_path)

url_pattern = %r{^(\s+)url "https://github\.com/akz142857/Halro/releases/download/[^/]+/(halro-(?:darwin|linux)-(?:amd64|arm64)\.tar\.gz)"\n\1sha256 "[0-9a-f]{64}"}
replacements = 0
formula.gsub!(url_pattern) do
  replacements += 1
  indent = Regexp.last_match(1)
  asset = Regexp.last_match(2)
  "#{indent}url \"https://github.com/akz142857/Halro/releases/download/#{version}/#{asset}\"\n" \
    "#{indent}sha256 \"#{checksums.fetch(asset)}\""
end
abort "expected four archive blocks, updated #{replacements}" unless replacements == 4

temporary = "#{formula_path}.tmp"
File.write(temporary, formula)
File.rename(temporary, formula_path)
