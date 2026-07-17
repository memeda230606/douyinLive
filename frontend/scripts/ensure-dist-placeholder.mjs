import { mkdir, writeFile } from 'node:fs/promises'

const outputDirectory = new URL('../dist/', import.meta.url)

await mkdir(outputDirectory, { recursive: true })
await writeFile(new URL('.gitkeep', outputDirectory), '')
