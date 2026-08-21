& C:\xnc\ffmpeg.exe -hide_banner -loglevel info -init_hw_device d3d11va -filter_complex "ddagrab=output_idx=0:framerate=5,hwdownload,format=bgra" -frames:v 3 C:\xnc\dda-%d.bmp *> C:\xnc\dda.out
exit $LASTEXITCODE
