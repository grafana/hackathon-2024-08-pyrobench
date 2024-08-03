const fs = require('fs');

const markdownTemplate = `# Benchmark Results

## Memory

<table>
  <thead>
    <tr>
      <td>Name</td>
      <td>Ref</td>
      <td>flamegraph.com</td>
      <td>Alloc objects (count)</td>
      <td>Alloc space (bytes)</td>
    </tr>
  </thead>

  <tbody>
    %MEMORY_ROWS%
  </tbody>
</table>

## CPU

<table>
  <thead>
    <tr>
      <td>Name</td>
      <td>Ref</td>
      <td>flamegraph.com</td>
      <td>CPU (ns)</td>
    </tr>
  </thead>

  <tbody>
    %CPU_ROWS%
  </tbody>
</table>`;

const memoryRowTemplate = `<tr>
  <td rowspan="3">%NAME%</td>
  <td>%BASE_REF% (base)</td>
  <td><a href="%BASE_MEMORY_URL%">%BASE_MEMORY_KEY%</a></td>
  <td align="right">%BASE_ALLOC_OBJECTS%</td>
  <td align="right">%BASE_ALLOC_SPACE%</td>
</tr>
<tr>
  <td>%HEAD_REF% (head)</td>
  <td><a href="%HEAD_MEMORY_URL%">%HEAD_MEMORY_KEY%</a></td>
  <td align="right">%HEAD_ALLOC_OBJECTS%</td>
  <td align="right">%HEAD_ALLOC_SPACE%</td>
</tr>
<tr>
  <td colspan="2" align="right"><strong>difference</strong></td>
  <td align="right">%DIFFERENCE_ALLOC_OBJECTS%</td>
  <td align="right">%DIFFERENCE_ALLOC_SPACE%</td>
</tr>`;

const cpuRowTemplate = `<tr>
  <td rowspan="3">%NAME%</td>
  <td>%BASE_REF% (base)</td>
  <td><a href="%BASE_CPU_URL%">%BASE_CPU_KEY%</a></td>
  <td align="right">%BASE_CPU%</td>
</tr>
<tr>
  <td>%HEAD_REF% (head)</td>
  <td><a href="%HEAD_CPU_URL%">%HEAD_CPU_KEY%</a></td>
  <td align="right">%HEAD_CPU%</td>
</tr>
<tr>
  <td colspan="2" align="right"><strong>difference</strong></td>
  <td align="right">%DIFFERENCE_CPU%</td>
</tr>`;

const partitionByName = (arr) => {
  const benchmarks = [];
  for (const { ref, type, benchmark } of arr) {
    const existing = benchmarks.find((b) => b.name === benchmark.Name);
    if (existing) {
      existing.runs.push({ ref, type, benchmark });
    } else {
      benchmarks.push({ name: benchmark.Name, runs: [{ ref, type, benchmark }] });
    }
  }

  return benchmarks;
};


const templateMemoryRow = (template, benchmark) => {
  const { name, runs } = benchmark;
  template = template.replaceAll(`%NAME%`, name);

  for (const { ref, type, benchmark: run } of runs) {
    const prefix = type.toUpperCase();
    template = template.replaceAll(`%${prefix}_REF%`, ref)
      .replaceAll(`%${prefix}_MEMORY_URL%`, run.AllocObjects.FlameGraphComURL)
      .replaceAll(`%${prefix}_MEMORY_KEY%`, run.AllocObjects.Key)
      .replaceAll(`%${prefix}_ALLOC_OBJECTS%`, run.AllocObjects.Total)
      .replaceAll(`%${prefix}_ALLOC_SPACE%`, run.AllocSpace.Total);
  }

  const { benchmark: base } = runs.find((run) => run.type === 'base');
  const { benchmark: head } = runs.find((run) => run.type === 'head');
  const difference = {
    allocObjects: head.AllocObjects.Total - base.AllocObjects.Total,
    allocSpace: head.AllocSpace.Total - base.AllocSpace.Total,
  };
  return template.replaceAll(`%DIFFERENCE_ALLOC_OBJECTS%`, difference.allocObjects)
    .replaceAll(`%DIFFERENCE_ALLOC_SPACE%`, difference.allocSpace);
};

const templateCpuRow = (template, benchmark) => {
  const { name, runs } = benchmark;
  template = template.replaceAll(`%NAME%`, name);

  for (const { ref, type, benchmark: run } of runs) {
    const prefix = type.toUpperCase();
    template = template.replaceAll(`%${prefix}_REF%`, ref)
      .replaceAll(`%${prefix}_CPU_URL%`, run.CPU.FlameGraphComURL)
      .replaceAll(`%${prefix}_CPU_KEY%`, run.CPU.Key)
      .replaceAll(`%${prefix}_CPU%`, run.CPU.Total);
  }

  const { benchmark: base } = runs.find((run) => run.type === 'base');
  const { benchmark: head } = runs.find((run) => run.type === 'head');
  const difference = {
    cpu: head.CPU.Total - base.CPU.Total,
  };
  return template.replaceAll(`%DIFFERENCE_CPU%`, difference.cpu);
};

const generateMarkdown = () => {
  const results = fs.readFileSync('results.txt', 'utf8')
    .split('\n')
    .filter((line) => line !== '')
    .map((line) => JSON.parse(line));


  let markdown = markdownTemplate.slice(0);
  const benchmarks = partitionByName(results);

  const memoryRows = [];
  const cpuRows = [];
  for (const benchmark of benchmarks) {
    memoryRows.push(templateMemoryRow(memoryRowTemplate.slice(0), benchmark));
    cpuRows.push(templateCpuRow(cpuRowTemplate.slice(0), benchmark));
  }

  // Add the memory and cpu rows to the markdown template.
  markdown = markdown
    .replaceAll(`%MEMORY_ROWS%`, memoryRows.join('\n'))
    .replaceAll(`%CPU_ROWS%`, cpuRows.join('\n'));
  return markdown;
};

module.exports = async ({ github, context }) => {
  const markdown = generateMarkdown();

  return await github.rest.issues.createComment({
    issue_number: context.issue.number,
    owner: context.repo.owner,
    repo: context.repo.repo,
    body: markdown,
  });
}
