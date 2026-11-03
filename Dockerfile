FROM scratch
COPY --chmod=0555 biology-library /biology-library
ENTRYPOINT ["/biology-library"]
CMD ["--library-page","/private/library.html","--data","/data","--listen","0.0.0.0:18765"]
