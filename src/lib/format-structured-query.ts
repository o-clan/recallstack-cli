import chalk from "chalk";

function dimLine(line: string): string {
  return chalk.dim(line);
}

function colorHeader(line: string): string {
  return chalk.cyanBright.bold(line);
}

function colorEvidenceScore(line: string): string {
  const match = line.match(/^(\d+\.\d{2})(\s+)(.+)$/);
  if (!match) return line;
  return `${chalk.cyanBright.bold(match[1])}${match[2]}${chalk.bold(match[3])}`;
}

export function formatStructuredQueryForCli(text: string): string {
  return text
    .split("\n")
    .map((line) => {
      if (line === "SYNTHESIS" || line === "STRATEGY" || line === "CONTEXT" || line === "EVIDENCE" || line === "CAVEATS") {
        return colorHeader(line);
      }
      if (/^\d+\.\d{2}\s+/.test(line)) {
        return colorEvidenceScore(line);
      }
      if (/^\s{4}/.test(line) || line.startsWith("- ")) {
        return dimLine(line);
      }
      return line;
    })
    .join("\n");
}
