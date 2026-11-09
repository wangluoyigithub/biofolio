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
icons = {'icons/biofolio.svg':'image/svg+xml','icons/apple-touch-icon.png':'image/png'}
for name, mime in icons.items():
    page = page.replace('href="'+name+'"', 'href="data:'+mime+';base64,'+base64.b64encode((root/'frontend'/name).read_bytes()).decode()+'"')
page = page.replace('<link rel="stylesheet" href="selection-ui.css">', '<style>' + (root/'frontend/selection-ui.css').read_text() + '</style>')
def inline(m):
    code=(root/'frontend'/m[1]).read_text()
    libs={'xlsx.full.min.js':'xlsx','index.iife.js':'docx','jszip.min.js':'zip','vue.global.prod.js':'vue'}
    lib=libs.get(Path(m[1]).name)
    tag=f'<script data-lib="{lib}">' if lib else '<script>'
    return tag+code.replace('</script','<\\/script')+'</script>'
page=re.sub(r'<script src="([^"]+)"></script>',inline,page)
viewer = ''.join('<script nonce="reader">'+(root/'third_party'/file).read_text().replace('</script','<\\/script')+'</script>' for file in ['jszip.min.js','docx-preview.min.js'])
head = '<script id="paper-data" type="application/json">'+json.dumps(seed,ensure_ascii=False).replace('<','\\u003c')+'</script>'
head += '<script id="word-libraries" type="text/plain">'+base64.b64encode(viewer.encode()).decode()+'</script>'
head += '<script id="pdf-libraries" type="text/plain">'+base64.b64encode((root/'third_party/pdfjs-4.10.38.zip').read_bytes()).decode()+'</script>'
args.out.write_text(page.replace('</head>',head+'</head>',1))
