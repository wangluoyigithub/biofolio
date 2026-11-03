"""Build a self-contained page from the bundled sources. Optional seed stays private."""
import argparse, base64, json, re
from pathlib import Path
root = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser()
parser.add_argument('--seed', type=Path)
parser.add_argument('--out', type=Path, default=Path('library.html'))
args = parser.parse_args()
seed = json.loads(args.seed.read_text()) if args.seed else dict(questions=[], papers=[], knowledge=[], key='biology-library-v1')
page = (root/'frontend/selection-preview.html').read_text()
def inline(m):
    code=(root/'frontend'/m[1]).read_text()
    return '<script>'+code.replace('</script','<\\/script')+'</script>'
page=re.sub(r'<script src="([^"]+)"></script>',inline,page)
viewer = ''.join('<script nonce="reader">'+(root/'third_party'/file).read_text().replace('</script','<\\/script')+'</script>' for file in ['jszip.min.js','docx-preview.min.js'])
head = '<script id="paper-data" type="application/json">'+json.dumps(seed,ensure_ascii=False).replace('<','\\u003c')+'</script>'
head += '<script id="word-libraries" type="text/plain">'+base64.b64encode(viewer.encode()).decode()+'</script>'
args.out.write_text(page.replace('</head>',head+'</head>',1))
