import test from 'node:test';
import assert from 'node:assert/strict';
import {artifactDraftProblem,mergeLibraryPages} from '../src/lib/wren-library.ts';
test('editing bounds account for UTF8 and encoded CSV quoting without evaluating formulas',()=>{
 assert.equal(artifactDraftProblem({kind:'text'},'猫'.repeat(349526),[]),'Keep this file under 1 MiB to edit it here.');
 assert.equal(artifactDraftProblem({kind:'csv',delimiter:',',lineEnding:'crlf'},'',[['=HYPERLINK("https://example.test")','<script>alert(1)</script>']]),'');
 assert.match(artifactDraftProblem({kind:'csv'},'',Array.from({length:2001},()=>['value'])),/2,000 rows/);
 assert.match(artifactDraftProblem({kind:'csv'},'',[Array(65).fill('')]),/64 columns/);
});
test('library paging keeps one actual file identity and accepts its newest metadata',()=>{
 const old={id:'a',revision:1},newer={id:'a',revision:2},other={id:'b',revision:1};
 assert.deepEqual(mergeLibraryPages([old],[newer,other]),[newer,other]);
});
